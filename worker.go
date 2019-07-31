package work

import (
	"fmt"
	"math/rand"
	"reflect"
	"time"

	"errors"
	"github.com/garyburd/redigo/redis"
)

const fetchKeysPerJobType = 6

type worker struct {
	workerID    string
	poolID      string
	namespace   string
	pool        *redis.Pool
	jobTypes    map[string]*jobType
	middleware  []*middlewareHandler
	hook        []*middlewareHandler
	contextType reflect.Type

	redisFetchScript *redis.Script
	sampler          prioritySampler
	*observer

	stopChan         chan struct{}
	doneStoppingChan chan struct{}

	drainChan        chan struct{}
	doneDrainingChan chan struct{}

	clearChan        chan struct{}
	doneClearingChan chan struct{}
}

func newWorker(namespace string, poolID string, pool *redis.Pool, contextType reflect.Type, middleware, hook []*middlewareHandler, jobTypes map[string]*jobType) *worker {
	workerID := makeIdentifier()
	ob := newObserver(namespace, pool, workerID)

	w := &worker{
		workerID:    workerID,
		poolID:      poolID,
		namespace:   namespace,
		pool:        pool,
		contextType: contextType,

		observer: ob,

		stopChan:         make(chan struct{}),
		doneStoppingChan: make(chan struct{}),

		drainChan:        make(chan struct{}),
		doneDrainingChan: make(chan struct{}),

		clearChan:        make(chan struct{}),
		doneClearingChan: make(chan struct{}),
	}

	w.updateMiddlewareAndJobTypes(middleware, hook, jobTypes)

	return w
}

// note: can't be called while the thing is started
func (w *worker) updateMiddlewareAndJobTypes(middleware, hook []*middlewareHandler, jobTypes map[string]*jobType) {
	if middleware != nil {
		w.middleware = middleware
	}
	if hook != nil {
		w.hook = hook
	}
	sampler := prioritySampler{}
	for _, jt := range jobTypes {
		sampler.add(jt.Priority,
			redisKeyJobs(w.namespace, jt.Name),
			redisKeyJobsInProgress(w.namespace, w.poolID, jt.Name),
			redisKeyJobsPaused(w.namespace, jt.Name),
			redisKeyJobsLock(w.namespace, jt.Name),
			redisKeyJobsLockInfo(w.namespace, jt.Name),
			redisKeyJobsConcurrency(w.namespace, jt.Name))
	}
	w.sampler = sampler
	w.jobTypes = jobTypes
	w.redisFetchScript = redis.NewScript(len(jobTypes)*fetchKeysPerJobType, redisLuaFetchJob)
}

func (w *worker) start() {
	go w.loop()
	go w.observer.start()
}

func (w *worker) stop() {
	w.stopChan <- struct{}{}
	<-w.doneStoppingChan
	w.observer.drain()
	w.observer.stop()
}

func (w *worker) drain() {
	w.drainChan <- struct{}{}
	<-w.doneDrainingChan
	w.observer.drain()
}

func (w *worker) ClearWorker() {
	w.clearChan <- struct{}{}
	<-w.doneClearingChan
}

var sleepBackoffsInMilliseconds = []int64{0, 10, 100, 1000, 5000}

func (w *worker) loop() {
	var drained bool
	var consequtiveNoJobs int64

	// Begin immediately. We'll change the duration on each tick with a timer.Reset()
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-w.stopChan:
			w.doneStoppingChan <- struct{}{}
			return
		case <-w.drainChan:
			drained = true
			timer.Reset(0)
		case <-timer.C:
			job, err := w.fetchJob()
			if err != nil {
				logError("worker.fetch", err)
				timer.Reset(10 * time.Millisecond)
			} else if job != nil {
				w.processJob(job)
				consequtiveNoJobs = 0
				timer.Reset(0)
			} else {
				if drained {
					w.doneDrainingChan <- struct{}{}
					drained = false
				}
				consequtiveNoJobs++
				idx := consequtiveNoJobs
				if idx >= int64(len(sleepBackoffsInMilliseconds)) {
					idx = int64(len(sleepBackoffsInMilliseconds)) - 1
				}
				timer.Reset(time.Duration(sleepBackoffsInMilliseconds[idx]) * time.Millisecond)
			}
		}
	}
}

func (w *worker) fetchJob() (*Job, error) {
	// resort queues
	// NOTE: we could optimize this to only resort every second, or something.
	w.sampler.sample()
	numKeys := len(w.sampler.samples) * fetchKeysPerJobType
	var scriptArgs = make([]interface{}, 0, numKeys+1)

	for _, s := range w.sampler.samples {
		scriptArgs = append(scriptArgs, s.redisJobs, s.redisJobsInProg, s.redisJobsPaused, s.redisJobsLock, s.redisJobsLockInfo, s.redisJobsMaxConcurrency) // KEYS[1-6 * N]
	}
	scriptArgs = append(scriptArgs, w.poolID) // ARGV[1]
	conn := w.pool.Get()
	defer conn.Close()

	values, err := redis.Values(w.redisFetchScript.Do(conn, scriptArgs...))
	if err == redis.ErrNil {
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	if len(values) != 3 {
		return nil, fmt.Errorf("need 3 elements back")
	}

	rawJSON, ok := values[0].([]byte)
	if !ok {
		return nil, fmt.Errorf("response msg not bytes")
	}

	dequeuedFrom, ok := values[1].([]byte)
	if !ok {
		return nil, fmt.Errorf("response queue not bytes")
	}

	inProgQueue, ok := values[2].([]byte)
	if !ok {
		return nil, fmt.Errorf("response in prog not bytes")
	}

	job, err := newJob(rawJSON, dequeuedFrom, inProgQueue)
	if err != nil {
		return nil, err
	}

	return job, nil
}

func (w *worker) processJob(job *Job) {
	defer func() {
		if job.Unique {
			w.deleteUniqueJob(job)
		}
	}()
	if jt, ok := w.jobTypes[job.Name]; ok {
		if jt.StartingDeadline > 0 && job.ScheduledAt > 0 && job.ScheduledAt < jt.StartingDeadline {
			w.removeJobFromInProgress(job)
			return
		}
		timeout := time.Duration(jt.Timeout) * time.Millisecond
		if timeout <= 0 {
			timeout = time.Minute * 1440 * 14
		}
		w.observeStarted(job.Name, job.ID, job.Args)
		job.observer = w.observer // for Checkin
		middleware := append(w.middleware, jt.middleware...)
		hook := append(w.hook, jt.hook...)
		var runErr error
		chErr := make(chan error)
		chCtx := make(chan reflect.Value)
		go func() {
			ctx, err := runJob(job, w.contextType, middleware, jt)
			chErr <- err
			chCtx <- ctx
		}()
		select {
		case <-time.After(timeout):
			if timeout > 0 {
				fmt.Printf("Job %s Timeout", job.Name)
				runErr = errors.New("Run Job Timeout")
				break
			}
		case runErr = <-chErr:
			ctx := <-chCtx
			if runErr != nil {
				job.Success = false
			} else {
				job.Success = true
			}
			runHook(job, ctx, hook)
			break
		case <-w.clearChan:
			w.doneClearingChan <- struct{}{}
			break
		}
		w.observeDone(job.Name, job.ID, runErr)
		if runErr != nil {
			job.failed(runErr)
			w.addToRetryOrDead(jt, job, runErr)
		} else {
			w.removeJobFromInProgress(job)
		}

	} else {
		// NOTE: since we don't have a jobType, we don't know max retries
		runErr := fmt.Errorf("stray job: no handler")
		logError("process_job.stray", runErr)
		job.failed(runErr)
		w.addToDead(job, runErr)
	}
}

func (w *worker) deleteUniqueJob(job *Job) {
	uniqueKey, err := redisKeyUniqueJob(w.namespace, job.Name, job.Args)
	if err != nil {
		logError("worker.delete_unique_job.key", err)
	}
	conn := w.pool.Get()
	defer conn.Close()

	_, err = conn.Do("DEL", uniqueKey)
	if err != nil {
		logError("worker.delete_unique_job.del", err)
	}
}

func (w *worker) removeJobFromInProgress(job *Job) {
	conn := w.pool.Get()
	defer conn.Close()

	// remove job from in progress and decr the lock in one transaction
	conn.Send("MULTI")
	conn.Send("LREM", job.inProgQueue, 1, job.rawJSON)
	conn.Send("DECR", redisKeyJobsLock(w.namespace, job.Name))
	conn.Send("HINCRBY", redisKeyJobsLockInfo(w.namespace, job.Name), w.poolID, -1)
	if _, err := conn.Do("EXEC"); err != nil {
		logError("worker.remove_job_from_in_progress.lrem", err)
	}
}

type NoRetryError struct {
	msg string
}

func (n *NoRetryError) Error() string {
	return n.msg
}

func (w *worker) addToRetryOrDead(jt *jobType, job *Job, runErr error) {
	_, isNoRetryError := runErr.(*NoRetryError)
	failsRemaining := int64(jt.MaxFails) - job.Fails
	if failsRemaining > 0 && !isNoRetryError {
		w.addToRetry(job, runErr)
	} else if !jt.SkipDead {
		w.addToDead(job, runErr)
	} else {
		w.removeJobFromInProgress(job)
	}
}

func (w *worker) addToRetry(job *Job, runErr error) {
	rawJSON, err := job.serialize()
	if err != nil {
		logError("worker.add_to_retry", err)
		return
	}

	conn := w.pool.Get()
	defer conn.Close()

	var backoff BackoffCalculator

	// Choose the backoff provider
	jt, ok := w.jobTypes[job.Name]
	if ok {
		backoff = jt.Backoff
	}

	if backoff == nil {
		backoff = defaultBackoffCalculator
	}

	conn.Send("MULTI")
	conn.Send("LREM", job.inProgQueue, 1, job.rawJSON)
	conn.Send("DECR", redisKeyJobsLock(w.namespace, job.Name))
	conn.Send("HINCRBY", redisKeyJobsLockInfo(w.namespace, job.Name), w.poolID, -1)
	conn.Send("ZADD", redisKeyRetry(w.namespace), nowEpochSeconds()+backoff(job), rawJSON)
	if _, err = conn.Do("EXEC"); err != nil {
		logError("worker.add_to_retry.exec", err)
	}
}

func (w *worker) addToDead(job *Job, runErr error) {
	rawJSON, err := job.serialize()

	if err != nil {
		logError("worker.add_to_dead.serialize", err)
		return
	}

	conn := w.pool.Get()
	defer conn.Close()

	// NOTE: sidekiq limits the # of jobs: only keep jobs for 6 months, and only keep a max # of jobs
	// The max # of jobs seems really horrible. Seems like operations should be on top of it.
	// conn.Send("ZREMRANGEBYSCORE", redisKeyDead(w.namespace), "-inf", now - keepInterval)
	// conn.Send("ZREMRANGEBYRANK", redisKeyDead(w.namespace), 0, -maxJobs)

	conn.Send("MULTI")
	conn.Send("LREM", job.inProgQueue, 1, job.rawJSON)
	conn.Send("DECR", redisKeyJobsLock(w.namespace, job.Name))
	conn.Send("HINCRBY", redisKeyJobsLockInfo(w.namespace, job.Name), w.poolID, -1)
	conn.Send("ZADD", redisKeyDead(w.namespace), nowEpochSeconds(), rawJSON)
	_, err = conn.Do("EXEC")
	if err != nil {
		logError("worker.add_to_dead.exec", err)
	}
}

// Default algorithm returns an fastly increasing backoff counter which grows in an unbounded fashion
func defaultBackoffCalculator(job *Job) int64 {
	fails := job.Fails
	return (fails * fails * fails * fails) + 15 + (rand.Int63n(30) * (fails + 1))
}

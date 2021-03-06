package work

import (
	"fmt"
	"github.com/garyburd/redigo/redis"
	"github.com/robfig/cron"
	"reflect"
	"sort"
	"strings"
	"sync"
)

// WorkerPool represents a pool of workers. It forms the primary API of gocraft/work. WorkerPools provide the public API of gocraft/work. You can attach jobs and middlware to them. You can start and stop them. Based on their concurrency setting, they'll spin up N worker goroutines.
type WorkerPool struct {
	workerPoolID string
	concurrency  uint
	namespace    string // eg, "myapp-work"
	pool         *redis.Pool

	contextType  reflect.Type
	jobTypes     map[string]*jobType
	middleware   []*middlewareHandler
	hook         []*middlewareHandler
	started      bool
	periodicJobs []*periodicJob

	workers          []*worker
	heartbeater      *workerPoolHeartbeater
	retrier          *requeuer
	scheduler        *requeuer
	deadPoolReaper   *deadPoolReaper
	periodicEnqueuer *periodicEnqueuer
}

type jobType struct {
	Name string
	JobOptions

	IsGeneric      bool
	GenericHandler GenericHandler
	DynamicHandler reflect.Value
	middleware     []*middlewareHandler
	hook           []*middlewareHandler
}

// You may provide your own backoff function for retrying failed jobs or use the builtin one.
// Returns the number of seconds to wait until the next attempt.
//
// The builtin backoff calculator provides an exponentially increasing wait function.
type BackoffCalculator func(job *Job) int64

// JobOptions can be passed to JobWithOptions.
type JobOptions struct {
	Priority         uint              // Priority from 1 to 10000
	MaxFails         uint              // 1: send straight to dead (unless SkipDead)
	SkipDead         bool              // If true, don't send failed jobs to the dead queue when retries are exhausted.
	MaxConcurrency   uint              // Max number of jobs to keep in flight (default is 0, meaning no max)
	Backoff          BackoffCalculator // If not set, uses the default backoff algorithm
	StartingDeadline int64             // UTC time in seconds(time.Now().Unix()), the deadline for starting the job if it misses its scheduled time for any reason
	RetryOnStart     bool              // If true, when a worker pool is started, jobs that are "in progress" will be retried
	Timeout          int
}

// GenericHandler is a job handler without any custom context.
type GenericHandler func(*Job) error

// GenericMiddlewareHandler is a middleware without any custom context.
type GenericMiddlewareHandler func(*Job, NextMiddlewareFunc) error

// NextMiddlewareFunc is a function type (whose instances are named 'next') that you call to advance to the next middleware.
type NextMiddlewareFunc func() error

type middlewareHandler struct {
	IsGeneric                bool
	DynamicMiddleware        reflect.Value
	GenericMiddlewareHandler GenericMiddlewareHandler
}

// NewWorkerPool creates a new worker pool. ctx should be a struct literal whose type will be used for middleware and handlers.
// concurrency specifies how many workers to spin up - each worker can process jobs concurrently.
func NewWorkerPool(ctx interface{}, concurrency uint, namespace string, pool *redis.Pool) *WorkerPool {
	if pool == nil {
		panic("NewWorkerPool needs a non-nil *redis.Pool")
	}

	ctxType := reflect.TypeOf(ctx)
	validateContextType(ctxType)
	wp := &WorkerPool{
		workerPoolID: makeIdentifier(),
		concurrency:  concurrency,
		namespace:    namespace,
		pool:         pool,
		contextType:  ctxType,
		jobTypes:     make(map[string]*jobType),
	}

	for i := uint(0); i < wp.concurrency; i++ {
		w := newWorker(wp.namespace, wp.workerPoolID, wp.pool, wp.contextType, nil, nil, wp.jobTypes)
		wp.workers = append(wp.workers, w)
	}
	wp.Job(fmt.Sprintf("%s:%s", "WorkerDrain", wp.workerPoolID), wp.workerDrain)
	return wp
}

func (wp *WorkerPool) workerDrain(job *Job) error {
	workerID := fmt.Sprint(job.Args["worker_id"])
	for _, v := range wp.workers {
		if v.workerID == workerID {
			v.drain()
		}
	}
	return nil
}

// Middleware appends the specified function to the middleware chain. The fn can take one of these forms:
// (*ContextType).func(*Job, NextMiddlewareFunc) error, (ContextType matches the type of ctx specified when creating a pool)
// func(*Job, NextMiddlewareFunc) error, for the generic middleware format.
func (wp *WorkerPool) Middleware(fn interface{}) *WorkerPool {
	return wp.Middlewares([]interface{}{fn})
}

// Middlewares appends the specified functions to the middleware chain. The fn in fns can take one of these forms:
// (*ContextType).func(*Job, NextMiddlewareFunc) error, (ContextType matches the type of ctx specified when creating a pool)
// func(*Job, NextMiddlewareFunc) error, for the generic middleware format.
func (wp *WorkerPool) Middlewares(fns []interface{}) *WorkerPool {
	wp.middleware = funcToMiddleware(fns, wp.contextType)

	for _, w := range wp.workers {
		w.updateMiddlewareAndJobTypes(wp.middleware, nil, wp.jobTypes)
	}

	return wp
}

// Middleware appends the specified function to the middleware chain. The fn can take one of these forms:
// (*ContextType).func(*Job, NextMiddlewareFunc) error, (ContextType matches the type of ctx specified when creating a pool)
// func(*Job, NextMiddlewareFunc) error, for the generic middleware format.
func (wp *WorkerPool) Hook(fn interface{}) *WorkerPool {
	return wp.Hooks([]interface{}{fn})
}

// Middlewares appends the specified functions to the middleware chain. The fn in fns can take one of these forms:
// (*ContextType).func(*Job, NextMiddlewareFunc) error, (ContextType matches the type of ctx specified when creating a pool)
// func(*Job, NextMiddlewareFunc) error, for the generic middleware format.
func (wp *WorkerPool) Hooks(fns []interface{}) *WorkerPool {
	wp.hook = funcToMiddleware(fns, wp.contextType)

	for _, w := range wp.workers {
		w.updateMiddlewareAndJobTypes(nil, wp.hook, wp.jobTypes)
	}

	return wp
}

// Job registers the job name to the specified handler fn. For instance, when workers pull jobs from the name queue they'll be processed by the specified handler function.
// fn can take one of these forms:
// (*ContextType).func(*Job) error, (ContextType matches the type of ctx specified when creating a pool)
// func(*Job) error, for the generic handler format.
func (wp *WorkerPool) Job(name string, fn interface{}) *WorkerPool {
	return wp.JobWithOptionsAndMiddlewares(name, JobOptions{}, fn, []interface{}{}, []interface{}{})
}

// JobWithOptions adds a handler for 'name' jobs as per the Job function, but permits you specify additional options
// such as a job's priority, retry count, and whether to send dead jobs to the dead job queue or trash them.
func (wp *WorkerPool) JobWithOptions(name string, jobOpts JobOptions, fn interface{}) *WorkerPool {
	return wp.JobWithOptionsAndMiddlewares(name, jobOpts, fn, []interface{}{}, []interface{}{})
}

// JobWithMiddlewares adds middlewares for 'name' jobs as per the Job specified middlware chain
func (wp *WorkerPool) JobWithMiddlewares(name string, fn interface{}, fns, hks []interface{}) *WorkerPool {
	return wp.JobWithOptionsAndMiddlewares(name, JobOptions{}, fn, fns, hks)
}

// JobWithOptionsAndMiddlewares adds a handler for 'name' jobs as per the Job function, but permits you specify additional options
// such as a job's priority, retry count, and whether to send dead jobs to the dead job queue or trash them.
// And adds middlewares for 'name' jobs as per the Job specified middlware chain
func (wp *WorkerPool) JobWithOptionsAndMiddlewares(name string, jobOpts JobOptions, fn interface{}, fns, hks []interface{}) *WorkerPool {
	jobOpts = applyDefaultsAndValidate(jobOpts)

	vfn := reflect.ValueOf(fn)
	validateHandlerType(wp.contextType, vfn)
	jobMiddleware := funcToMiddleware(fns, wp.contextType)
	hookMiddleware := funcToMiddleware(hks, wp.contextType)

	jt := &jobType{
		Name:           name,
		DynamicHandler: vfn,
		JobOptions:     jobOpts,
		middleware:     jobMiddleware,
		hook:           hookMiddleware,
	}
	if gh, ok := fn.(func(*Job) error); ok {
		jt.IsGeneric = true
		jt.GenericHandler = gh
	}

	wp.jobTypes[name] = jt

	for _, w := range wp.workers {
		w.updateMiddlewareAndJobTypes(wp.middleware, wp.hook, wp.jobTypes)
	}

	return wp
}

// PeriodicallyEnqueue will periodically enqueue jobName according to the cron-based spec.
// The spec format is based on https://godoc.org/github.com/robfig/cron, which is a relatively standard cron format.
// Note that the first value is the seconds!
// If you have multiple worker pools on different machines, they'll all coordinate and only enqueue your job once.
func (wp *WorkerPool) PeriodicallyEnqueue(spec string, jobName string) *WorkerPool {
	schedule, err := cron.Parse(spec)
	if err != nil {
		panic(err)
	}

	wp.periodicJobs = append(wp.periodicJobs, &periodicJob{jobName: jobName, spec: spec, schedule: schedule})

	return wp
}

// Start starts the workers and associated processes.
func (wp *WorkerPool) Start() {
	if wp.started {
		return
	}
	wp.started = true

	// TODO: we should cleanup stale keys on startup from previously registered jobs
	wp.writeConcurrencyControlsToRedis()
	go wp.writeKnownJobsToRedis()

	for _, w := range wp.workers {
		go w.start()
	}

	wp.heartbeater = newWorkerPoolHeartbeater(wp.namespace, wp.pool, wp.workerPoolID, wp.jobTypes, wp.concurrency, wp.workerIDs())
	wp.heartbeater.start()
	wp.startRequeuers()
	wp.periodicEnqueuer = newPeriodicEnqueuer(wp.namespace, wp.pool, wp.periodicJobs)
	wp.periodicEnqueuer.start()
}

// Stop stops the workers and associated processes.
func (wp *WorkerPool) Stop() {
	if !wp.started {
		return
	}
	wp.started = false

	wg := sync.WaitGroup{}
	for _, w := range wp.workers {
		wg.Add(1)
		go func(w *worker) {
			w.stop()
			wg.Done()
		}(w)
	}
	wg.Wait()
	wp.heartbeater.stop()
	wp.retrier.stop()
	wp.scheduler.stop()
	wp.deadPoolReaper.stop()
	wp.periodicEnqueuer.stop()
}

// Drain drains all jobs in the queue before returning. Note that if jobs are added faster than we can process them, this function wouldn't return.
func (wp *WorkerPool) Drain() {
	wg := sync.WaitGroup{}
	for _, w := range wp.workers {
		wg.Add(1)
		go func(w *worker) {
			w.drain()
			wg.Done()
		}(w)
	}
	wg.Wait()
}

func (wp *WorkerPool) startRequeuers() {
	jobNames := make([]string, 0, len(wp.jobTypes))
	for k := range wp.jobTypes {
		jobNames = append(jobNames, k)
	}
	wp.retrier = newRequeuer(wp.namespace, wp.pool, redisKeyRetry(wp.namespace), jobNames)
	wp.scheduler = newRequeuer(wp.namespace, wp.pool, redisKeyScheduled(wp.namespace), jobNames)
	wp.deadPoolReaper = newDeadPoolReaper(wp.namespace, wp.pool, jobNames, wp.jobTypes)
	wp.retrier.start()
	wp.scheduler.start()
	wp.deadPoolReaper.start()
}

func (wp *WorkerPool) workerIDs() []string {
	wids := make([]string, 0, len(wp.workers))
	for _, w := range wp.workers {
		wids = append(wids, w.workerID)
	}
	sort.Strings(wids)
	return wids
}

func (wp *WorkerPool) writeKnownJobsToRedis() {
	if len(wp.jobTypes) == 0 {
		return
	}

	conn := wp.pool.Get()
	defer conn.Close()
	key := redisKeyKnownJobs(wp.namespace)
	jobNames := make([]interface{}, 0, len(wp.jobTypes)+1)
	jobNames = append(jobNames, key)
	for k := range wp.jobTypes {
		jobNames = append(jobNames, k)
	}

	if _, err := conn.Do("SADD", jobNames...); err != nil {
		logError("write_known_jobs", err)
	}
}

func (wp *WorkerPool) writeConcurrencyControlsToRedis() {
	if len(wp.jobTypes) == 0 {
		return
	}

	conn := wp.pool.Get()
	defer conn.Close()
	for jobName, jobType := range wp.jobTypes {
		if _, err := conn.Do("SET", redisKeyJobsConcurrency(wp.namespace, jobName), jobType.MaxConcurrency); err != nil {
			logError("write_concurrency_controls_max_concurrency", err)
		}
	}
}

func funcToMiddleware(fns []interface{}, ctxType reflect.Type) []*middlewareHandler {
	var middleware []*middlewareHandler
	for _, fn := range fns {
		vfn := reflect.ValueOf(fn)
		validateMiddlewareType(ctxType, vfn)

		mw := &middlewareHandler{
			DynamicMiddleware: vfn,
		}

		if gmh, ok := fn.(func(*Job, NextMiddlewareFunc) error); ok {
			mw.IsGeneric = true
			mw.GenericMiddlewareHandler = gmh
		}

		middleware = append(middleware, mw)
	}

	return middleware
}

// validateContextType will panic if context is invalid
func validateContextType(ctxType reflect.Type) {
	if ctxType.Kind() != reflect.Struct {
		panic("work: Context needs to be a struct type")
	}
}

func validateHandlerType(ctxType reflect.Type, vfn reflect.Value) {
	if !isValidHandlerType(ctxType, vfn) {
		panic(instructiveMessage(vfn, "a handler", "handler", "job *work.Job", ctxType))
	}
}

func validateMiddlewareType(ctxType reflect.Type, vfn reflect.Value) {
	if !isValidMiddlewareType(ctxType, vfn) {
		panic(instructiveMessage(vfn, "middleware", "middleware", "job *work.Job, next NextMiddlewareFunc", ctxType))
	}
}

// Since it's easy to pass the wrong method as a middleware/handler, and since the user can't rely on static type checking since we use reflection,
// lets be super helpful about what they did and what they need to do.
// Arguments:
//  - vfn is the failed method
//  - addingType is for "You are adding {addingType} to a worker pool...". Eg, "middleware" or "a handler"
//  - yourType is for "Your {yourType} function can have...". Eg, "middleware" or "handler" or "error handler"
//  - args is like "rw web.ResponseWriter, req *web.Request, next web.NextMiddlewareFunc"
//    - NOTE: args can be calculated if you pass in each type. BUT, it doesn't have example argument name, so it has less copy/paste value.
func instructiveMessage(vfn reflect.Value, addingType string, yourType string, args string, ctxType reflect.Type) string {
	// Get context type without package.
	ctxString := ctxType.String()
	splitted := strings.Split(ctxString, ".")
	if len(splitted) <= 1 {
		ctxString = splitted[0]
	} else {
		ctxString = splitted[1]
	}

	str := "\n" + strings.Repeat("*", 120) + "\n"
	str += "* You are adding " + addingType + " to a worker pool with context type '" + ctxString + "'\n"
	str += "*\n*\n"
	str += "* Your " + yourType + " function can have one of these signatures:\n"
	str += "*\n"
	str += "* // If you don't need context:\n"
	str += "* func YourFunctionName(" + args + ") error\n"
	str += "*\n"
	str += "* // If you want your " + yourType + " to accept a context:\n"
	str += "* func (c *" + ctxString + ") YourFunctionName(" + args + ") error  // or,\n"
	str += "* func YourFunctionName(c *" + ctxString + ", " + args + ") error\n"
	str += "*\n"
	str += "* Unfortunately, your function has this signature: " + vfn.Type().String() + "\n"
	str += "*\n"
	str += strings.Repeat("*", 120) + "\n"

	return str
}

func isValidHandlerType(ctxType reflect.Type, vfn reflect.Value) bool {
	fnType := vfn.Type()

	if fnType.Kind() != reflect.Func {
		return false
	}

	numIn := fnType.NumIn()
	numOut := fnType.NumOut()

	if numOut != 1 {
		return false
	}

	outType := fnType.Out(0)
	var e *error

	if outType != reflect.TypeOf(e).Elem() {
		return false
	}

	var j *Job
	if numIn == 1 {
		if fnType.In(0) != reflect.TypeOf(j) {
			return false
		}
	} else if numIn == 2 {
		if fnType.In(0) != reflect.PtrTo(ctxType) {
			return false
		}
		if fnType.In(1) != reflect.TypeOf(j) {
			return false
		}
	} else {
		return false
	}

	return true
}

func isValidMiddlewareType(ctxType reflect.Type, vfn reflect.Value) bool {
	fnType := vfn.Type()

	if fnType.Kind() != reflect.Func {
		return false
	}

	numIn := fnType.NumIn()
	numOut := fnType.NumOut()

	if numOut != 1 {
		return false
	}

	outType := fnType.Out(0)
	var e *error

	if outType != reflect.TypeOf(e).Elem() {
		return false
	}

	var j *Job
	var nfn NextMiddlewareFunc
	if numIn == 2 {
		if fnType.In(0) != reflect.TypeOf(j) {
			return false
		}
		if fnType.In(1) != reflect.TypeOf(nfn) {
			return false
		}
	} else if numIn == 3 {
		if fnType.In(0) != reflect.PtrTo(ctxType) {
			return false
		}
		if fnType.In(1) != reflect.TypeOf(j) {
			return false
		}
		if fnType.In(2) != reflect.TypeOf(nfn) {
			return false
		}
	} else {
		return false
	}

	return true
}

func applyDefaultsAndValidate(jobOpts JobOptions) JobOptions {
	if jobOpts.Priority == 0 {
		jobOpts.Priority = 1
	}

	if jobOpts.MaxFails == 0 {
		jobOpts.MaxFails = 4
	}

	if jobOpts.Priority > 100000 {
		panic("work: JobOptions.Priority must be between 1 and 100000")
	}

	return jobOpts
}

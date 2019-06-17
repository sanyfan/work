package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"time"
	"errors"
	"github.com/FZambia/go-sentinel"
	"github.com/garyburd/redigo/redis"
	"github.com/sanyfan/work/webui"
	"strings"
)

var (
	redisHostPort  = flag.String("redis", ":6379", "redis hostport")
	redisDatabase  = flag.String("database", "0", "redis database")
	redisNamespace = flag.String("ns", "work", "redis namespace")
	webHostPort    = flag.String("listen", ":5040", "hostport to listen for HTTP JSON API")
	redisSentinelHosts  = flag.String("sentinel", "", "redis sentinel hostport")
)

func main() {
	flag.Parse()

	fmt.Println("Starting workwebui:")
	fmt.Println("redis = ", *redisHostPort)
	fmt.Println("redis sentinel = ", *redisSentinelHosts)
	fmt.Println("database = ", *redisDatabase)
	fmt.Println("namespace = ", *redisNamespace)
	fmt.Println("listen = ", *webHostPort)

	database, err := strconv.Atoi(*redisDatabase)
	if err != nil {
		fmt.Printf("Error: %v is not a valid database value", *redisDatabase)
		return
	}

	pool ,err := createPool(*redisHostPort,  *redisSentinelHosts,database)
	if err != nil {
		fmt.Printf("Error: create redis pool err: %v", err)
		return
	}

	server := webui.NewServer(*redisNamespace, pool, *webHostPort)
	server.Start()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, os.Kill)

	<-c

	server.Stop()

	fmt.Println("\nQuitting...")
}


func sentinelHosts(sentinelHostStr string) []string {
	if strings.TrimSpace(sentinelHostStr) == "" {
		return []string{}
	}
	return strings.Split(sentinelHostStr, ",")
}

func sentinelDialFunc(hosts []string,db int) func() (redis.Conn, error) {
	sntnl := &sentinel.Sentinel{
		Addrs:      hosts,
		MasterName: "mymaster",
		Dial: func(addr string) (redis.Conn, error) {
			timeout := 500 * time.Millisecond
			c, err := redis.Dial("tcp", addr,
				redis.DialReadTimeout(timeout), redis.DialWriteTimeout(timeout), redis.DialConnectTimeout(timeout),redis.DialDatabase(db))
			if err != nil {
				return nil, err
			}
			return c, nil
		},
	}
	return func() (redis.Conn, error) {
		masterAddr, err := sntnl.MasterAddr()
		if err != nil {
			return nil, err
		}
		c, err := redis.Dial("tcp", masterAddr)
		fmt.Println("redis master address: " + masterAddr)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
}


func createPool(addr,sentinel string,database int) (*redis.Pool,error) {
	dialFunc := func() (redis.Conn, error) { return nil, nil }
	if len(sentinel) > 0 {
		sentinelHosts := sentinelHosts(sentinel)
		dialFunc = sentinelDialFunc(sentinelHosts,database)
	} else if len(*redisHostPort) > 0 {
		dialFunc = func() (redis.Conn, error) {
			return redis.DialURL(addr, redis.DialDatabase(database))
		}
	} else {
		return nil, errors.New("invalid sentinel hosts and host + port")
	}
	return &redis.Pool{
		MaxActive: 3,
		MaxIdle:   3,
		Wait:      true,
		Dial:      dialFunc,
	},nil
}

func newPool(addr string, database int) *redis.Pool {
	return &redis.Pool{
		MaxActive:   3,
		MaxIdle:     3,
		IdleTimeout: 240 * time.Second,
		Dial: func() (redis.Conn, error) {
			return redis.DialURL(addr, redis.DialDatabase(database))
		},
		Wait: true,
	}
}

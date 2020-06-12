package gue

import (
	"bytes"
	"fmt"
	"log"
	"runtime"
	"sync"
	"time"
)

const (
	defaultWakeInterval = 5 * time.Second
	defaultQueueName    = ""
)

// WorkFunc is a function that performs a Job. If an error is returned, the job
// is re-enqueued with exponential backoff.
type WorkFunc func(j *Job) error

// WorkMap is a map of Job names to WorkFuncs that are used to perform Jobs of a
// given type.
type WorkMap map[string]WorkFunc

// Worker is a single worker that pulls jobs off the specified Queue. If no Job
// is found, the Worker will sleep for Interval seconds.
type Worker struct {
	// Interval is the amount of time that this Worker should sleep before trying
	// to find another Job.
	Interval time.Duration

	// Queue is the name of the queue to pull Jobs off of. The default value, "",
	// is usable and is the default for both que-go and the ruby que library.
	Queue string

	c *Client
	m WorkMap

	mu   sync.Mutex
	done bool
	ch   chan struct{}
}

// NewWorker returns a Worker that fetches Jobs from the Client and executes
// them using WorkMap. If the type of Job is not registered in the WorkMap, it's
// considered an error and the job is re-enqueued with a backoff.
//
// Workers default to an Interval of 5 seconds, which can be overridden by
// WakeInterval option.
// The default Queue is the nameless queue "", which can be overridden by
// WorkerQueue option. Also these settings can be changed on the returned
// Worker before it is started with Work().
func NewWorker(c *Client, m WorkMap, options ...WorkerOption) *Worker {
	instance := Worker{
		Interval: defaultWakeInterval,
		Queue:    defaultQueueName,
		c:        c,
		m:        m,
		ch:       make(chan struct{}),
	}

	for _, option := range options {
		option(&instance)
	}

	return &instance
}

// Work pulls jobs off the Worker's Queue at its Interval. This function only
// returns after Shutdown() is called, so it should be run in its own goroutine.
func (w *Worker) Work() {
	defer log.Println("worker done")
	for {
		// Try to work a job
		if w.WorkOne() {
			// Since we just did work, non-blocking check whether we should exit
			select {
			case <-w.ch:
				return
			default:
				// continue in loop
			}
		} else {
			// No work found, block until exit or timer expires
			select {
			case <-w.ch:
				return
			case <-time.After(w.Interval):
				// continue in loop
			}
		}
	}
}

// WorkOne tries to consume single message from the queue.
func (w *Worker) WorkOne() (didWork bool) {
	j, err := w.c.LockJob(w.Queue)
	if err != nil {
		log.Printf("attempting to lock job: %v", err)
		return
	}
	if j == nil {
		return // no job was available
	}
	defer j.Done()
	defer recoverPanic(j)

	didWork = true

	wf, ok := w.m[j.Type]
	if !ok {
		msg := fmt.Sprintf("unknown job type: %q", j.Type)
		log.Println(msg)
		if err = j.Error(msg); err != nil {
			log.Printf("attempting to save error on job %d: %v", j.ID, err)
		}
		return
	}

	if err = wf(j); err != nil {
		if jErr := j.Error(err.Error()); jErr != nil {
			log.Printf("got an error (%v) when tried to mark job as errored (%v)", jErr, err)
		}
		return
	}

	if err = j.Delete(); err != nil {
		log.Printf("attempting to delete job %d: %v", j.ID, err)
	}
	log.Printf("event=job_worked job_id=%d job_type=%s", j.ID, j.Type)
	return
}

// Shutdown tells the worker to finish processing its current job and then stop.
// There is currently no timeout for in-progress jobs. This function blocks
// until the Worker has stopped working. It should only be called on an active
// Worker.
func (w *Worker) Shutdown() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.done {
		return
	}

	log.Println("worker shutting down gracefully...")
	w.ch <- struct{}{}
	w.done = true
	close(w.ch)
}

// recoverPanic tries to handle panics in job execution.
// A stacktrace is stored into Job last_error.
func recoverPanic(j *Job) {
	if r := recover(); r != nil {
		// record an error on the job with panic message and stacktrace
		stackBuf := make([]byte, 1024)
		n := runtime.Stack(stackBuf, false)

		buf := &bytes.Buffer{}
		fmt.Fprintf(buf, "%v\n", r)
		fmt.Fprintln(buf, string(stackBuf[:n]))
		fmt.Fprintln(buf, "[...]")
		stacktrace := buf.String()
		log.Printf("event=panic job_id=%d job_type=%s\n%s", j.ID, j.Type, stacktrace)
		if err := j.Error(stacktrace); err != nil {
			log.Printf("attempting to save error on job %d: %v", j.ID, err)
		}
	}
}

// WorkerPool is a pool of Workers, each working jobs from the queue Queue
// at the specified Interval using the WorkMap.
type WorkerPool struct {
	WorkMap  WorkMap
	Interval time.Duration
	Queue    string

	c       *Client
	workers []*Worker
	mu      sync.Mutex
	done    bool
}

// NewWorkerPool creates a new WorkerPool with count workers using the Client c.
//
// Each Worker in the pool default to an Interval of 5 seconds, which can be
// overridden by PoolWakeInterval option. The default Queue is the
// nameless queue "", which can be overridden by PoolWorkerQueue option. Also
// these settings can be changed on the returned WorkerPool before it is started
// with Start().
func NewWorkerPool(c *Client, wm WorkMap, count int, options ...WorkerPoolOption) *WorkerPool {
	instance := WorkerPool{
		c:        c,
		WorkMap:  wm,
		Interval: defaultWakeInterval,
		workers:  make([]*Worker, count),
	}

	for _, option := range options {
		option(&instance)
	}

	return &instance
}

// Start starts all of the Workers in the WorkerPool.
func (w *WorkerPool) Start() {
	w.mu.Lock()
	defer w.mu.Unlock()

	for i := range w.workers {
		w.workers[i] = NewWorker(w.c, w.WorkMap)
		w.workers[i].Interval = w.Interval
		w.workers[i].Queue = w.Queue
		go w.workers[i].Work()
	}
}

// Shutdown sends a Shutdown signal to each of the Workers in the WorkerPool and
// waits for them all to finish shutting down.
func (w *WorkerPool) Shutdown() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.done {
		return
	}
	var wg sync.WaitGroup
	wg.Add(len(w.workers))

	for _, worker := range w.workers {
		go func(worker *Worker) {
			// If Shutdown is called before Start has been called,
			// then these are nil, so don't try to close them
			if worker != nil {
				worker.Shutdown()
			}
			wg.Done()
		}(worker)
	}
	wg.Wait()
	w.done = true
}

// Simple live charts for memory consumption and GC pauses.
//
// To use debugcharts, link this package into your program:
//	import _ "github.com/mkevac/debugcharts"
//
// If your application is not already running an http DebugChartServer, you
// need to start one.  Add "net/http" and "log" to your imports and
// the following code to your main function:
//
// 	go func() {
// 		log.Println(http.ListenAndServe("localhost:6060", nil))
// 	}()
//
// Then go look at charts:
//
//	http://localhost:6060/debug/charts
//
package debugcharts

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"runtime/pprof"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/mkevac/debugcharts/bindata"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/process"
)

const (
	maxCount                  int = 86400
	DefaultDebugChartsPattern     = "/debug/charts/"
)

type update struct {
	Ts             int64
	BytesAllocated uint64
	GcPause        uint64
	CPUUser        float64
	CPUSys         float64
	Block          int
	Goroutine      int
	Heap           int
	Mutex          int
	Threadcreate   int
}

type consumer struct {
	id uint
	c  chan update
}

type SimplePair struct {
	Ts    uint64
	Value uint64
}

type CPUPair struct {
	Ts   uint64
	User float64
	Sys  float64
}

type PprofPair struct {
	Ts           uint64
	Block        int
	Goroutine    int
	Heap         int
	Mutex        int
	Threadcreate int
}

type DataStorage struct {
	BytesAllocated []SimplePair
	GcPauses       []SimplePair
	CPUUsage       []CPUPair
	Pprof          []PprofPair
}

type DebugChartServer struct {
	consumers      []consumer
	mux            *http.ServeMux
	pattern        string
	server         http.Server
	consumersMutex sync.RWMutex
	log            Logger
	errorChan      chan error

	data           DataStorage
	lastPause      uint32
	mutex          sync.RWMutex
	lastConsumerID uint
	upgrader       websocket.Upgrader
	prevSysTime    float64
	prevUserTime   float64
	myProcess      *process.Process
}

func NewDebugChartServer(pattern string, log Logger) *DebugChartServer {
	dcs := NewDebugChartService(http.DefaultServeMux, pattern, log)
	dcs.server = http.Server{Addr: "localhost:8080"}
	return dcs
}

//NewDebugChartService binds to existent mux with the given pattern
func NewDebugChartService(mux *http.ServeMux, pattern string, log Logger) *DebugChartServer {
	return (&DebugChartServer{mux: mux, pattern: pattern, log: log}).bind()
}

var (
	upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}
)

func (p *DebugChartServer) serve() error {
	go func() {
		if err := p.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			p.log.Printf("profiler server failed starting: %v", err)
			p.errorChan <- err
		}
	}()
	return nil
}

func (p *DebugChartServer) Shutdown() error {
	return p.server.Close()
}

func (p *DebugChartServer) gatherData() {
	timer := time.Tick(time.Second)

	for {
		select {
		case now := <-timer:
			nowUnix := now.Unix()

			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)

			u := update{
				Ts:           nowUnix * 1000,
				Block:        pprof.Lookup("block").Count(),
				Goroutine:    pprof.Lookup("goroutine").Count(),
				Heap:         pprof.Lookup("heap").Count(),
				Mutex:        pprof.Lookup("mutex").Count(),
				Threadcreate: pprof.Lookup("threadcreate").Count(),
			}
			p.data.Pprof = append(p.data.Pprof, PprofPair{
				uint64(nowUnix) * 1000,
				u.Block,
				u.Goroutine,
				u.Heap,
				u.Mutex,
				u.Threadcreate,
			})

			cpuTimes, err := p.myProcess.Times()
			if err != nil {
				cpuTimes = &cpu.TimesStat{}
			}

			if p.prevUserTime != 0 {
				u.CPUUser = cpuTimes.User - p.prevUserTime
				u.CPUSys = cpuTimes.System - p.prevSysTime
				p.data.CPUUsage = append(p.data.CPUUsage, CPUPair{uint64(nowUnix) * 1000, u.CPUUser, u.CPUSys})
			}

			p.prevUserTime = cpuTimes.User
			p.prevSysTime = cpuTimes.System

			p.mutex.Lock()

			bytesAllocated := ms.Alloc
			u.BytesAllocated = bytesAllocated
			p.data.BytesAllocated = append(p.data.BytesAllocated, SimplePair{uint64(nowUnix) * 1000, bytesAllocated})
			if p.lastPause == 0 || p.lastPause != ms.NumGC {
				gcPause := ms.PauseNs[(ms.NumGC+255)%256]
				u.GcPause = gcPause
				p.data.GcPauses = append(p.data.GcPauses, SimplePair{uint64(nowUnix) * 1000, gcPause})
				p.lastPause = ms.NumGC
			}

			if len(p.data.BytesAllocated) > maxCount {
				p.data.BytesAllocated = p.data.BytesAllocated[len(p.data.BytesAllocated)-maxCount:]
			}

			if len(p.data.GcPauses) > maxCount {
				p.data.GcPauses = p.data.GcPauses[len(p.data.GcPauses)-maxCount:]
			}

			p.mutex.Unlock()

			p.sendToConsumers(u)
		}
	}
}

func (p *DebugChartServer) getUri(suffix string) string {
	uri := p.pattern
	if uri[len(uri)-1] != '/' {
		uri += "/"
	}
	uri += suffix
	return uri
}
func (p *DebugChartServer) bind() *DebugChartServer {
	p.mux.HandleFunc(p.getUri("data-feed"), p.dataFeedHandler)
	p.mux.HandleFunc(p.getUri("data"), p.dataHandler)

	p.mux.HandleFunc(p.getUri(""), handleAsset("static/index.html"))

	p.mux.HandleFunc(p.getUri("main.js"), handleAsset("static/main.js"))
	p.mux.HandleFunc(p.getUri("jquery-2.1.4.min.js"), handleAsset("static/jquery-2.1.4.min.js"))
	p.mux.HandleFunc(p.getUri("moment.min.js"), handleAsset("static/moment.min.js"))

	p.myProcess, _ = process.NewProcess(int32(os.Getpid()))

	// preallocate arrays in data, helps save on reallocation caused by append()
	// when maxCount is large
	p.data.BytesAllocated = make([]SimplePair, 0, maxCount)
	p.data.GcPauses = make([]SimplePair, 0, maxCount)
	p.data.CPUUsage = make([]CPUPair, 0, maxCount)
	p.data.Pprof = make([]PprofPair, 0, maxCount)

	go p.gatherData()
	return p
}

func (p *DebugChartServer) sendToConsumers(u update) {
	p.consumersMutex.RLock()
	defer p.consumersMutex.RUnlock()

	for _, c := range p.consumers {
		c.c <- u
	}
}

func (p *DebugChartServer) removeConsumer(id uint) {
	p.consumersMutex.Lock()
	defer p.consumersMutex.Unlock()

	var consumerID uint
	var consumerFound bool

	for i, c := range p.consumers {
		if c.id == id {
			consumerFound = true
			consumerID = uint(i)
			break
		}
	}

	if consumerFound {
		p.consumers = append(p.consumers[:consumerID], p.consumers[consumerID+1:]...)
	}
}

func (p *DebugChartServer) addConsumer() consumer {
	p.consumersMutex.Lock()
	defer p.consumersMutex.Unlock()

	p.lastConsumerID++

	c := consumer{
		id: p.lastConsumerID,
		c:  make(chan update),
	}

	p.consumers = append(p.consumers, c)

	return c
}

func (p *DebugChartServer) dataFeedHandler(w http.ResponseWriter, r *http.Request) {
	var (
		lastPing time.Time
		lastPong time.Time
	)

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	conn.SetPongHandler(func(s string) error {
		lastPong = time.Now()
		return nil
	})

	// read and discard all messages
	go func(c *websocket.Conn) {
		for {
			if _, _, err := c.NextReader(); err != nil {
				c.Close()
				break
			}
		}
	}(conn)

	c := p.addConsumer()

	defer func() {
		p.removeConsumer(c.id)
		conn.Close()
	}()

	var i uint

	for u := range c.c {
		websocket.WriteJSON(conn, u)
		i++

		if i%10 == 0 {
			if diff := lastPing.Sub(lastPong); diff > time.Second*60 {
				return
			}
			now := time.Now()
			if err := conn.WriteControl(websocket.PingMessage, nil, now.Add(time.Second)); err != nil {
				return
			}
			lastPing = now
		}
	}
}

func (p *DebugChartServer) dataHandler(w http.ResponseWriter, r *http.Request) {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	if e := r.ParseForm(); e != nil {
		log.Print("error parsing form")
		return
	}

	callback := r.FormValue("callback")

	fmt.Fprintf(w, "%v(", callback)

	w.Header().Set("Content-Type", "application/json")

	encoder := json.NewEncoder(w)
	encoder.Encode(p.data)

	fmt.Fprint(w, ")")
}

func handleAsset(path string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := bindata.Asset(path)
		if err != nil {
			log.Print(err)
			return
		}

		n, err := w.Write(data)
		if err != nil {
			log.Print(err)
			return
		}

		if n != len(data) {
			log.Print("wrote less than supposed to")
			return
		}
	}
}

type Logger interface {
	Printf(format string, v ...interface{})
	Print(v ...interface{})
	Println(v ...interface{})
	Fatal(v ...interface{})
	Fatalf(format string, v ...interface{})
	Fatalln(v ...interface{})
	Panic(v ...interface{})
	Panicf(format string, v ...interface{})
	Panicln(v ...interface{})
}

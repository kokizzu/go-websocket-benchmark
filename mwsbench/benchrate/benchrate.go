package benchrate

import (
	"bytes"
	"context"
	"crypto/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"go-websocket-benchmark/config"
	"go-websocket-benchmark/logging"
	"go-websocket-benchmark/mwsbench/protocol"
	"go-websocket-benchmark/mwsbench/report"

	"github.com/lesismal/nbio/nbhttp/websocket"
	"github.com/lesismal/perf"
	"golang.org/x/time/rate"
)

type BenchRate struct {
	Framework   string
	Ip          string
	Duration    time.Duration
	Concurrency int
	SendRate    int
	Payload     int
	SendLimit   int
	PsInterval  time.Duration

	ServerPid int
	PsCounter *perf.PSCounter

	ConnsMap map[*websocket.Conn]struct{}

	wbuffer []byte

	chConns chan *websocket.Conn

	limitFn func()

	batch       int
	batchBuffer []byte
	tickRate    int

	sendTimes int64
	sendBytes int64
	recvTimes int64
	recvBytes int64
}

type Conn struct {
	net.Conn
	sendCnt int64
	recvCnt int64
}

func New(framework string, ip string, connsMap map[*websocket.Conn]struct{}) *BenchRate {
	bm := &BenchRate{
		Framework: framework,
		Ip:        ip,
		ConnsMap:  connsMap,
		limitFn:   func() {},
	}
	return bm
}

func (br *BenchRate) Run() {
	br.init()
	defer br.clean()

	chCounterStart := make(chan struct{})
	go func() {
		br.PsCounter.Start(perf.PSCountOptions{
			CountCPU: true,
			CountMEM: true,
			CountIO:  true,
			CountNET: true,
			Interval: br.PsInterval,
		})
		time.Sleep(br.PsInterval)
		close(chCounterStart)
	}()

	done := make(chan struct{})
	time.AfterFunc(br.Duration, func() {
		close(done)
	})

	logging.Printf("BenchRate for %.2f seconds ...", br.Duration.Seconds())

	wg := sync.WaitGroup{}

	connTeams := make([][]*Conn, br.Concurrency)
	cnt := 0
	for wsc := range br.ConnsMap {
		cnt++
		idx := cnt % len(connTeams)
		conn := &Conn{
			Conn: wsc.Conn,
		}
		connTeams[idx] = append(connTeams[idx], conn)
		wsc.SetSession(conn)
	}
	for i := 0; i < br.Concurrency; i++ {
		wg.Add(1)
		conns := connTeams[i]
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(time.Second / time.Duration(br.tickRate))
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					br.doOnce(conns)
				}
			}
		}()
	}
	wg.Wait()

	logging.Printf("BenchRate for %.2f seconds done", br.Duration.Seconds())

	<-chCounterStart
	br.PsCounter.Stop()
}

func (br *BenchRate) Stop() {

}

func (br *BenchRate) Report() report.Report {
	return &report.BenchRateReport{
		Framework:   br.Framework,
		Duration:    br.Duration.Nanoseconds(),
		Connections: len(br.ConnsMap),
		SendRate:    br.SendRate,
		Payload:     br.Payload,
		SendTimes:   br.sendTimes,
		SendBytes:   br.sendBytes,
		RecvTimes:   br.recvTimes,
		RecvBytes:   br.recvBytes,
		CPUMin:      br.PsCounter.CPUMin(),
		CPUAvg:      br.PsCounter.CPUAvg(),
		CPUMax:      br.PsCounter.CPUMax(),
		MEMRSSMin:   br.PsCounter.MEMRSSMin(),
		MEMRSSAvg:   br.PsCounter.MEMRSSAvg(),
		MEMRSSMax:   br.PsCounter.MEMRSSMax(),
	}
}

func (br *BenchRate) init() {
	if br.Duration <= 0 {
		br.Duration = time.Second * 10
	}
	if br.Concurrency <= 0 {
		br.Concurrency = 50000
	}
	if br.Concurrency > len(br.ConnsMap) {
		br.Concurrency = len(br.ConnsMap)
	}
	if br.SendRate <= 0 {
		br.SendRate = 1
	}
	if br.Payload <= 0 {
		br.Payload = 1024
	}

	br.wbuffer = make([]byte, br.Payload)
	rand.Read(br.wbuffer)
	message := protocol.EncodeClientMessage(websocket.BinaryMessage, br.wbuffer)
	br.batchBuffer, br.batch, br.tickRate = protocol.BatchBuffers(message, br.SendRate, 1024*8)
	// br.batchBuffer, br.batch, br.tickRate = message, 1, br.SendRate
	if br.tickRate <= 0 || len(br.batchBuffer) == 0 {
		logging.Fatalf("BenchRate get wrong tickRate: %v, or batchBuffer: %v", br.tickRate, len(br.batchBuffer))
	}

	if br.PsInterval <= 0 {
		br.PsInterval = time.Second
	}

	if br.SendLimit > 0 {
		limiter := rate.NewLimiter(rate.Every(1*time.Second), br.SendLimit)
		br.limitFn = func() {
			limiter.WaitN(context.Background(), len(br.batchBuffer)/br.Payload)
		}
	}

	br.chConns = make(chan *websocket.Conn, len(br.ConnsMap)*br.SendRate)
	for c := range br.ConnsMap {
		c.OnMessage(br.onMessage)
	}
	for i := 0; i < br.SendRate; i++ {
		for c := range br.ConnsMap {
			br.chConns <- c
		}
	}

	serverPid, err := config.GetFrameworkPid(br.Framework, br.Ip)
	if err != nil {
		logging.Fatalf("BenchRate GetFrameworkPid(%v) failed: %v", br.Framework, err)
	}
	br.ServerPid = serverPid
	psCounter, err := perf.NewPSCounter(serverPid)
	if err != nil {
		panic(err)
	}
	br.PsCounter = psCounter
}

func (br *BenchRate) clean() {
	br.chConns = nil
	br.limitFn = func() {}
}

func (br *BenchRate) getWriteBuffer() []byte {
	return br.wbuffer
}

func (br *BenchRate) doOnce(conns []*Conn) {
	for _, conn := range conns {
		if atomic.LoadInt64(&conn.sendCnt)-atomic.LoadInt64(&conn.recvCnt)+int64(br.batch) < int64(br.batch*5) {
			br.limitFn()
			_, err := conn.Write(br.batchBuffer)
			if err == nil {
				atomic.AddInt64(&br.sendTimes, int64(br.batch))
				atomic.AddInt64(&br.sendBytes, int64(br.batch*br.Payload))
				atomic.AddInt64(&conn.sendCnt, int64(br.batch))
			}
		}
	}
}

func (br *BenchRate) onMessage(c *websocket.Conn, mt websocket.MessageType, b []byte) {
	if mt == websocket.BinaryMessage && bytes.Equal(b, br.getWriteBuffer()) {
		conn := c.Session().(*Conn)
		atomic.AddInt64(&conn.recvCnt, 1)
		atomic.AddInt64(&br.recvTimes, 1)
		atomic.AddInt64(&br.recvBytes, int64(len(b)))
	}
}

package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/encoding"
	xxhash "github.com/cespare/xxhash/v2"
	jump "github.com/lithammer/go-jump-consistent-hash"
	"github.com/silenceper/pool"
	log "github.com/sirupsen/logrus"
)

// 记录连接池、Buff池信息，以作后续复用
type proxyConnPool struct {
	carbonRelay []string
	tcpConnPools map[string]pool.Pool
	writeBufPools map[string]*sync.Pool
}

// 当前连接状态信息
type connStatusInfo struct {
	currConn map[string]net.Conn
	currBuff map[string]*bufio.Writer
}

var carbonRelayPool *proxyConnPool

func main() {
	var (
		logLevel, listenAddr, remoteAddr string
		debugMode bool
	)

	flag.StringVar(&logLevel, "logLevel", "info", "log level")
	flag.StringVar(&listenAddr, "listenAddr", ":2004", "listen address")
	flag.StringVar(&remoteAddr, "remoteAddr", "127.0.0.1:2004,127.0.0.2:2004", "carbon-c-relay's address")
	flag.BoolVar(&debugMode, "debugMode", false, "whether to enable debug mode")
	flag.Parse()

	carbonRelayPool = &proxyConnPool{
		carbonRelay: strings.Split(remoteAddr, ","),
		tcpConnPools: make(map[string]pool.Pool),
		writeBufPools: make(map[string]*sync.Pool),
	}

	fmt.Printf("Reverse proxy to: %s\n", carbonRelayPool.carbonRelay)

	// 针对每个cerbon-c-relay，建立一个TCP连接池
	for _, ipAddr := range carbonRelayPool.carbonRelay {
		if newPool, err := newTcpConnPool(ipAddr); err == nil {
			carbonRelayPool.tcpConnPools[ipAddr] = newPool
		}
	}

	level, err := log.ParseLevel(logLevel)
	if err != nil {
		log.Fatal(err)
	}
	log.SetLevel(level)

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatal(err)
	}

	readerPool := &sync.Pool{
		New: func() interface{} {
			return bufio.NewReaderSize(nil, 64*1024)
		},
	}

	// 针对每个后端的carbon-c-relay，建立一个bufpool
	for _, ipAddr := range carbonRelayPool.carbonRelay {
		carbonRelayPool.writeBufPools[ipAddr] = &sync.Pool{
			New: func() interface{} {
				return bufio.NewWriterSize(nil, 64*1024)
			},
		}
	}

	builderPool := &sync.Pool{
		New: func() interface{} {
			return bytes.NewBuffer(make([]byte, 1024))
		},
	}

	ch := make(chan os.Signal)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGUSR1)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Errorf("accept failed %s", err)
			continue
		}

		go func(localConn net.Conn) {
			defer func() {
				if r := recover(); r != nil {
					log.Errorf("catch panic %s", r)
				}
			}()

			defer func() { _ = localConn.Close() }()

			reader := readerPool.Get().(*bufio.Reader)
			defer readerPool.Put(reader)
			reader.Reset(localConn)

			currConnStatus := &connStatusInfo{
				make(map[string]net.Conn),
				make(map[string]*bufio.Writer),
			}

			for _, ipAddr := range carbonRelayPool.carbonRelay {
				remoteConn, _ := carbonRelayPool.tcpConnPools[ipAddr].Get()
				writerPool := carbonRelayPool.writeBufPools[ipAddr]

				writer := writerPool.Get().(*bufio.Writer)
				writer.Reset(remoteConn.(net.Conn))

				currConnStatus.currConn[ipAddr] = remoteConn.(net.Conn)
				currConnStatus.currBuff[ipAddr] = writer
			}

			defer func() {
				for tmpIp, tmpConn := range currConnStatus.currConn {
					_ = carbonRelayPool.tcpConnPools[tmpIp].Put(tmpConn)
					_ = currConnStatus.currBuff[tmpIp].Flush()
					carbonRelayPool.writeBufPools[tmpIp].Put(currConnStatus.currBuff[tmpIp])
				}
			}()

			builder := builderPool.Get().(*bytes.Buffer)
			defer builderPool.Put(builder)

			var next []byte
			for {
				line, isContinue, err := reader.ReadLine()
				for isContinue && err == nil {
					next, isContinue, err = reader.ReadLine()
					line = append(line, next...)
				}

				if err == io.EOF {
					return
				}

				if err != nil {
					log.Errorf("read from %s failed %s", localConn.RemoteAddr(), err)
					return
				}

				builder.Reset()
				idx, success := GetRemoteAddrIdx(builder, line)
				if !success {
					log.Debugf("ignore invalid metric %s", line)
					continue
				}

				tmpWrite := currConnStatus.currBuff[carbonRelayPool.carbonRelay[idx]]
				tmpConn := currConnStatus.currConn[carbonRelayPool.carbonRelay[idx]]
				if debugMode {
					fmt.Println(tmpConn.RemoteAddr(), idx)
				}

				_, err = tmpWrite.Write(builder.Bytes())
				if err != nil {
					log.Errorf("write to %s failed %s", tmpConn.RemoteAddr(), err)
					return
				}

				if tmpWrite.Available() < 8192 {
					err = tmpWrite.Flush()
				}
				if err != nil {
					log.Errorf("write to %s failed %s", tmpConn.RemoteAddr(), err)
					return
				}
			}
		}(conn)

		go func() {
			for {
				sig := <-ch
				if sig == syscall.SIGINT || sig == syscall.SIGTERM || sig == syscall.SIGQUIT {
					conn.Close()
					// 释放连接池中的所有连接
					for _, pool := range carbonRelayPool.tcpConnPools {
						pool.Release()
					}
					time.Sleep(2 * time.Second)
					os.Exit(1)
				}
			}
		}()
	}
}

func GetRemoteAddrIdx(builder *bytes.Buffer, line []byte) (int, bool) {
	i1 := bytes.IndexByte(line, ' ')
	if i1 < 0 {
		return -1, false
	}

	metricName := line[:i1]

	metricBuf := make([]byte, 0, 16)
	metricBuf = encoding.MarshalUint16(metricBuf, uint16(len(metricName)))
	metricBuf = append(metricBuf, metricName...)
	h := xxhash.Sum64(metricBuf)
	idx := int(jump.Hash(h, int32(len(carbonRelayPool.carbonRelay))))

	// 不做内容处理，直接转发
	builder.Write(line)
	builder.WriteByte('\n')

	return idx, true
}

func newTcpConnPool(ipAddr string) (pool.Pool, error) {
	// factory 创建连接的方法
	factory := func() (interface{}, error) { return net.Dial("tcp", ipAddr) }

	// close 关闭连接的方法
	close := func(v interface{}) error { return v.(net.Conn).Close() }

	// 连接有效性检查
	// ping := func(v interface{}) error { return nil }

	// 创建一个连接池：初始化5，最大连接10，空闲连接数是20
	poolConfig := &pool.Config{
		InitialCap: 5,
		MaxIdle:    10,
		MaxCap:     20,
		Factory:    factory,
		Close:      close,
		//Ping:     	ping,
		// 连接最大空闲时间，超过该时间的连接将会关闭，可避免空闲时连接EOF，自动失效的问题
		IdleTimeout: 15 * time.Second,
	}
	newPool, err := pool.NewChannelPool(poolConfig)
	if err != nil {
		log.Errorf("create tcp connetcion pool to %s is failed, %s", ipAddr, err)
		return nil, err
	}
	return newPool, nil
}

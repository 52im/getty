/******************************************************
# DESC       : getty client
# MAINTAINER : Alex Stocks
# LICENCE    : Apache License 2.0
# EMAIL      : alexstocks@foxmail.com
# MOD        : 2016-09-01 21:32
# FILE       : client.go
******************************************************/

package getty

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net"
	"strings"
	"sync"
	"time"
)

import (
	"encoding/pem"
	"github.com/AlexStocks/goext/sync"
	log "github.com/AlexStocks/log4go"
	"github.com/gorilla/websocket"
)

const (
	defaultInterval = 3e9 // 3s
	connectTimeout  = 5e9
	maxTimes        = 10
)

/////////////////////////////////////////
// getty tcp client
/////////////////////////////////////////

type Client struct {
	// net
	sync.Mutex
	number     int
	interval   time.Duration
	addr       string
	newSession NewSessionCallback
	ssMap      map[Session]gxsync.Empty

	sync.Once
	done chan gxsync.Empty
	wg   sync.WaitGroup

	// for wss client
	cert string // 服务端的证书文件（包含了公钥以及服务端其他一些验证信息：服务端域名、服务端ip、起始有效日期、有效时长、hash算法、秘钥长度等）
}

// NewClient function builds a tcp & ws client.
// @connNum is connection number.
// @connInterval is reconnect sleep interval when getty fails to connect the server.
// @serverAddr is server address.
func NewClient(connNum int, connInterval time.Duration, serverAddr string) *Client {
	if connNum < 0 {
		connNum = 1
	}
	if connInterval < defaultInterval {
		connInterval = defaultInterval
	}

	return &Client{
		number:   connNum,
		interval: connInterval,
		addr:     serverAddr,
		ssMap:    make(map[Session]gxsync.Empty, connNum),
		done:     make(chan gxsync.Empty),
	}
}

// NewClient function builds a wss client.
// @connNum is connection number.
// @connInterval is reconnect sleep interval when getty fails to connect the server.
// @serverAddr is server address.
// @cert is client certificate file. it can be emtpy.
// @privateKey is client private key(contains its public key). it can be empty.
// @caCert is the root certificate file to verify the legitimacy of server
func NewWSSClient(
	connNum int,
	connInterval time.Duration,
	serverAddr string,
	cert string,
) *Client {

	if connNum < 0 {
		connNum = 1
	}
	if connInterval < defaultInterval {
		connInterval = defaultInterval
	}

	return &Client{
		number:   connNum,
		interval: connInterval,
		addr:     serverAddr,
		ssMap:    make(map[Session]gxsync.Empty, connNum),
		done:     make(chan gxsync.Empty),
		cert:     cert,
	}
}

func (c *Client) dialTCP() Session {
	var (
		err  error
		conn net.Conn
	)

	for {
		if c.IsClosed() {
			return nil
		}
		conn, err = net.DialTimeout("tcp", c.addr, connectTimeout)
		if err == nil && conn.LocalAddr().String() == conn.RemoteAddr().String() {
			err = errSelfConnect
		}
		if err == nil {
			return NewTCPSession(conn)
		}

		log.Info("net.DialTimeout(addr:%s, timeout:%v) = error{%v}", c.addr, err)
		time.Sleep(c.interval)
		continue
	}
}

func (c *Client) dialWS() Session {
	var (
		err    error
		dialer websocket.Dialer
		conn   *websocket.Conn
		ss     Session
	)

	dialer.EnableCompression = true
	for {
		if c.IsClosed() {
			return nil
		}
		conn, _, err = dialer.Dial(c.addr, nil)
		log.Info("websocket.dialer.Dial(addr:%s) = error{%v}", c.addr, err)
		if err == nil && conn.LocalAddr().String() == conn.RemoteAddr().String() {
			err = errSelfConnect
		}
		if err == nil {
			ss = NewWSSession(conn)
			if ss.(*session).maxMsgLen > 0 {
				conn.SetReadLimit(int64(ss.(*session).maxMsgLen))
			}

			return ss
		}

		log.Info("websocket.dialer.Dial(addr:%s) = error{%v}", c.addr, err)
		time.Sleep(c.interval)
		continue
	}
}

func (c *Client) dialWSS() Session {
	var (
		err      error
		root     *x509.Certificate
		roots    []*x509.Certificate
		certPool *x509.CertPool
		config   *tls.Config
		dialer   websocket.Dialer
		conn     *websocket.Conn
		ss       Session
	)

	dialer.EnableCompression = true

	config = &tls.Config{
		InsecureSkipVerify: true,
	}

	if c.cert != "" {
		certPEMBlock, err := ioutil.ReadFile(c.cert)
		if err != nil {
			panic(fmt.Sprintf("ioutil.ReadFile(cert:%s) = error{%#v}", c.cert, err))
		}

		var cert tls.Certificate
		for {
			var certDERBlock *pem.Block
			certDERBlock, certPEMBlock = pem.Decode(certPEMBlock)
			if certDERBlock == nil {
				break
			}
			if certDERBlock.Type == "CERTIFICATE" {
				cert.Certificate = append(cert.Certificate, certDERBlock.Bytes)
			}
		}
		config.Certificates = make([]tls.Certificate, 1)
		config.Certificates[0] = cert
	}

	certPool = x509.NewCertPool()
	for _, c := range config.Certificates {
		roots, err = x509.ParseCertificates(c.Certificate[len(c.Certificate)-1])
		if err != nil {
			panic(fmt.Sprintf("error parsing server's root cert: %v\n", err))
		}
		for _, root = range roots {
			certPool.AddCert(root)
		}
	}
	config.InsecureSkipVerify = true
	config.RootCAs = certPool

	// dialer.EnableCompression = true
	dialer.TLSClientConfig = config
	for {
		if c.IsClosed() {
			return nil
		}
		conn, _, err = dialer.Dial(c.addr, nil)
		if err == nil && conn.LocalAddr().String() == conn.RemoteAddr().String() {
			err = errSelfConnect
		}
		if err == nil {
			ss = NewWSSession(conn)
			if ss.(*session).maxMsgLen > 0 {
				conn.SetReadLimit(int64(ss.(*session).maxMsgLen))
			}
			ss.SetName(defaultWSSSessionName)

			return ss
		}

		log.Info("websocket.dialer.Dial(addr:%s) = error{%v}", c.addr, err)
		time.Sleep(c.interval)
		continue
	}
}

func (c *Client) dial() Session {
	if strings.HasPrefix(c.addr, "wss") {
		return c.dialWSS()
	}

	if strings.HasPrefix(c.addr, "ws") {
		return c.dialWS()
	}

	return c.dialTCP()
}

func (c *Client) sessionNum() int {
	var num int

	c.Lock()
	for s := range c.ssMap {
		if s.IsClosed() {
			delete(c.ssMap, s)
		}
	}
	num = len(c.ssMap)
	c.Unlock()

	return num
}

func (c *Client) connect() {
	var (
		err error
		ss  Session
	)

	for {
		ss = c.dial()
		if ss == nil {
			// client has been closed
			break
		}
		err = c.newSession(ss)
		if err == nil {
			// ss.RunEventLoop()
			ss.(*session).run()
			c.Lock()
			c.ssMap[ss] = gxsync.Empty{}
			c.Unlock()
			break
		}
		// don't distinguish between tcp connection and websocket connection. Because
		// gorilla/websocket/conn.go:(Conn)Close also invoke net.Conn.Close()
		ss.Conn().Close()
	}
}

func (c *Client) RunEventLoop(newSession NewSessionCallback) {
	c.Lock()
	c.newSession = newSession
	c.Unlock()

	c.wg.Add(1)
	go func() {
		var num, max, times int
		defer c.wg.Done()

		c.Lock()
		max = c.number
		c.Unlock()
		// log.Info("maximum client connection number:%d", max)
		for {
			if c.IsClosed() {
				log.Warn("client{peer:%s} goroutine exit now.", c.addr)
				break
			}

			num = c.sessionNum()
			// log.Info("current client connction number:%d", num)
			if max <= num {
				times++
				if maxTimes < times {
					times = maxTimes
				}
				time.Sleep(time.Duration(int64(times) * defaultInterval))
				continue
			}
			times = 0
			c.connect()
			// time.Sleep(c.interval) // build c.number connections asap
		}
	}()
}

func (c *Client) stop() {
	select {
	case <-c.done:
		return
	default:
		c.Once.Do(func() {
			close(c.done)
			c.Lock()
			for s := range c.ssMap {
				s.Close()
			}
			c.ssMap = nil
			c.Unlock()
		})
	}
}

func (c *Client) IsClosed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *Client) Close() {
	c.stop()
	c.wg.Wait()
}

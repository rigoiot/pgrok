package server

import (
	"io"
	"net"
	"ngrok/conn"
	"ngrok/msg"
	"runtime/debug"
	"sync/atomic"
	"time"
)

const (
	pingTimeoutInterval = 30 * time.Second
	connReapInterval    = 10 * time.Second
)

type Control struct {
	// actual connection
	conn conn.Conn

	// channels for communicating messages over the connection
	out  chan (interface{})
	in   chan (msg.Message)
	stop chan (msg.Message)

	// heartbeat
	lastPing int64

	// tunnel
	tun *Tunnel
}

func NewControl(tcpConn *net.TCPConn) {
	c := &Control{
		conn:     conn.NewTCP(tcpConn, "ctl"),
		out:      make(chan (interface{}), 1),
		in:       make(chan (msg.Message), 1),
		stop:     make(chan (msg.Message), 1),
		lastPing: time.Now().Unix(),
	}

	go c.managerThread()
	go c.readThread()
}

func (c *Control) managerThread() {
	reap := time.NewTicker(connReapInterval)

	// all shutdown functionality in here
	defer func() {
		if err := recover(); err != nil {
			c.conn.Info("Control::managerThread failed with error %v: %s", err, debug.Stack())
		}
		reap.Stop()
		c.conn.Close()

		// shutdown the tunnel if it's open
		if c.tun != nil {
			c.tun.shutdown()
		}
	}()

	for {
		select {
		case m := <-c.out:
			msg.WriteMsg(c.conn, m)

		case <-reap.C:
			lastPing := time.Unix(c.lastPing, 0)
			if time.Since(lastPing) > pingTimeoutInterval {
				c.conn.Info("Lost heartbeat")
				metrics.lostHeartbeatMeter.Mark(1)
				return
			}

		case m := <-c.stop:
			if m != nil {
				msg.WriteMsg(c.conn, m)
			}
			return

		case mRaw := <-c.in:
			switch m := mRaw.(type) {
			case *msg.RegMsg:
				c.conn.Info("Registering new tunnel")
				c.tun = newTunnel(m, c)

			case *msg.PingMsg:
				atomic.StoreInt64(&c.lastPing, time.Now().Unix())
				c.out <- &msg.PongMsg{}

			case *msg.VersionMsg:
				c.out <- &msg.VersionRespMsg{Version: version}
			}
		}
	}
}

func (c *Control) readThread() {
	defer func() {
		if err := recover(); err != nil {
			c.conn.Info("Control::readThread failed with error %v: %s", err, debug.Stack())
		}
		c.stop <- nil
	}()

	// read messages from the control channel
	for {
		if msg, err := msg.ReadMsg(c.conn); err != nil {
			if err == io.EOF {
				c.conn.Info("EOF")
				return
			} else {
				panic(err)
			}
		} else {
			c.in <- msg
		}
	}
}
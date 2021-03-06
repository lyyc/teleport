// Copyright 2015-2017 HenryLee. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package teleport

import (
	"crypto/tls"
	"net"
	"sync"
	"time"

	"github.com/henrylee2cn/goutil/errors"
)

// Peer peer which is server or client.
type Peer struct {
	*ApiMap
	id              string
	pluginContainer PluginContainer
	sessionHub      *SessionHub
	idMaker         IdMaker
	closeCh         chan struct{}
	freeContext     *ApiContext
	ctxLock         sync.Mutex
	readTimeout     time.Duration // readdeadline for underlying net.Conn
	writeTimeout    time.Duration // writedeadline for underlying net.Conn
	tlsConfig       *tls.Config
	slowApiDuration time.Duration
	mu              sync.Mutex

	// for client role
	dialTimeout time.Duration

	// for server role
	listenAddrs []string
	listens     []net.Listener
}

// ErrListenClosed listener closed error.
var ErrListenClosed = errors.New("teleport: listener closed")

// NewPeer creates a new peer.
func NewPeer(cfg *Config) *Peer {
	var p = &Peer{
		id:              cfg.Id,
		ApiMap:          newApiMap(),
		pluginContainer: newPluginContainer(),
		idMaker:         newIdMaker(),
		sessionHub:      newSessionHub(),
		readTimeout:     cfg.ReadTimeout,
		writeTimeout:    cfg.WriteTimeout,
		closeCh:         make(chan struct{}),
		slowApiDuration: cfg.SlowApiDuration,
		dialTimeout:     cfg.DialTimeout,
		listenAddrs:     cfg.ListenAddrs,
	}
	return p
}

func (p *Peer) SetIdMaker(idMaker IdMaker) {
	p.idMaker = idMaker
}

func (p *Peer) ServeConn(conn net.Conn, id ...string) *Session {
	var session = newSession(p, conn, id...)
	p.sessionHub.Set(session.Id(), session)
	return session
}

func (p *Peer) Dial(addr string) (*Session, error) {
	var conn, err = net.DialTimeout("tcp", addr, p.dialTimeout)
	if err != nil {
		return nil, err
	}
	if p.tlsConfig != nil {
		conn = tls.Client(conn, p.tlsConfig)
	}
	sess := p.ServeConn(conn)
	return sess, nil
}

func (p *Peer) Listen() error {
	var (
		wg    sync.WaitGroup
		count = len(p.listenAddrs)
		errCh = make(chan error, count)
	)
	wg.Add(count)
	for _, addr := range p.listenAddrs {
		go func() {
			defer wg.Done()
			errCh <- p.listen(addr)
		}()
	}
	wg.Wait()
	close(errCh)
	var errs error
	for err := range errCh {
		e := err
		errs = errors.Append(errs, e)
	}
	return errs
}

func (p *Peer) listen(addr string) error {
	var (
		err error
		lis net.Listener
	)
	if p.tlsConfig != nil {
		lis, err = tls.Listen("tcp", addr, p.tlsConfig)
	} else {
		lis, err = net.Listen("tcp", addr)
	}
	if err != nil {
		Fatalf("%v", err)
	}
	p.listens = append(p.listens, lis)
	defer lis.Close()

	var (
		tempDelay time.Duration // how long to sleep on accept failure
		closeCh   = p.closeCh
	)
	for {
		rw, e := lis.Accept()
		if e != nil {
			select {
			case <-closeCh:
				return ErrListenClosed
			default:
			}
			if ne, ok := e.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				Tracef("teleport: Accept error: %v; retrying in %v", e, tempDelay)
				time.Sleep(tempDelay)
				continue
			}
			return e
		}
		tempDelay = 0
		p.ServeConn(rw)
	}
}

func (p *Peer) Close() {
	close(p.closeCh)
}

func (p *Peer) getContext(s *Session) *ApiContext {
	p.ctxLock.Lock()
	ctx := p.freeContext
	if ctx == nil {
		ctx = newApiContext()
	} else {
		p.freeContext = ctx.next
		ctx.reInit(s)
	}
	p.ctxLock.Unlock()
	return ctx
}

func (p *Peer) putContext(ctx *ApiContext) {
	p.ctxLock.Lock()
	ctx.clean()
	ctx.next = p.freeContext
	p.freeContext = ctx
	p.ctxLock.Unlock()
}

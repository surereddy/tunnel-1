package engine

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io/ioutil"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type Tun struct {
	stream  chan FromConn
	address string
	mode    string
	item    RouterItem
	router  *Router
	def     RouterItem

	secpath string
}

func (t *Tun) SetAddress(mode string, address string) {
	t.mode = mode
	t.address = address
}

func (t *Tun) SetRouterItem(item RouterItem) {
	t.item = item
}

func (t *Tun) SetDefault(def RouterItem) {
	log.Println("SetDefault:", def)
	t.def = def
}

func (t *Tun) SetSecPath(secpath string) {
	t.secpath = secpath
}

func (t *Tun) SetRouter(router *Router) {
	t.router = router
}

func (t *Tun) ClientWrapper() {

	var lastConnect time.Time
	var duration time.Duration = time.Second
	for {
		lastConnect = time.Now()

		conn, err := t.ClientDial("tcp", t.address)
		if err != nil {
			log.Println("Tun Dial:", err, t.address)
			duration *= 2
			time.Sleep(duration)
			continue
		}

		conn.(*net.TCPConn).SetKeepAlive(true)
		conn.(*net.TCPConn).SetKeepAlivePeriod(time.Duration(60) * time.Second)

		loop := NewTunLoop(conn, t.stream, t.router)

		if len(t.item.Domains) > 0 {
			item := t.item
			item.network = NewTunNetwork(loop)
			t.router.AddRouter(item)
		}

		if len(t.def.Domains) > 0 {
			loop.Register(t.def)
		}

		loop.Run()

		log.Println("Conn", t.address, "disconnected")

		// Less than 20 minutes running time considered a failure
		if time.Since(lastConnect).Minutes() < 20 {
			duration *= 2
			time.Sleep(duration)
			continue
		}

		duration = time.Second
	}
}

func (t *Tun) ClientDial(network string, address string) (net.Conn, error) {
	if t.secpath == "" {
		log.Println("Client Running...")
		return net.Dial(network, address)
	}

	cert, err := tls.LoadX509KeyPair(t.secpath+"/client.pem", t.secpath+"/client.key")
	if err != nil {
		log.Println("LoadX509 fail:", t.secpath+"/client.pem", err)
		return nil, err
	}

	log.Println("Tls Client Running...")
	log.Println("Try Connect", t.address)
	config := tls.Config{Certificates: []tls.Certificate{cert}}
	config.InsecureSkipVerify = true
	// config.ClientAuth = tls.VerifyClientCertIfGiven
	return tls.Dial(network, address, &config)
}

func (t *Tun) SeverListen(network string, address string) (net.Listener, error) {
	if t.secpath == "" {
		log.Println("Server Running...")
		return net.Listen(network, address)
	}

	cert, err := tls.LoadX509KeyPair(t.secpath+"/server.pem", t.secpath+"/server.key")
	if err != nil {
		log.Println("LoadX509 fail:", t.secpath+"/server.pem", err)
		return nil, err
	}

	cabytes, err := ioutil.ReadFile(t.secpath + "/ca.pem")
	if err != nil {
		log.Println("LoadX509 ca:", t.secpath+"/ca.pem", err)
		return nil, err
	}

	log.Println("Tls Server Running...")
	config := tls.Config{Certificates: []tls.Certificate{cert}}
	config.ClientCAs = x509.NewCertPool()
	ok := config.ClientCAs.AppendCertsFromPEM(cabytes)
	if !ok {
		return nil, errors.New("AppendCertsFromPEM failed")
	}

	config.ClientAuth = tls.RequireAndVerifyClientCert

	return tls.Listen(network, address, &config)
}

func (t *Tun) Run(stream chan FromConn) {
	t.stream = stream

	if t.mode == "Client" {
		go t.ClientWrapper()
	} else {
		ln, err := t.SeverListen("tcp", t.address)
		if err != nil {
			log.Println("SeverListen:", err, t.address)
			return
		}

		for {
			conn, err := ln.Accept()
			if err == nil {
				log.Println("One Client Enter:",
					conn.RemoteAddr().Network()+"@"+conn.RemoteAddr().String())
				conn.(*net.TCPConn).SetKeepAlive(true)
				conn.(*net.TCPConn).SetKeepAlivePeriod(time.Duration(60) * time.Second)
				go NewTunLoop(conn, t.stream, t.router).Run()
			} else {
				log.Println("Client Accept Failed:", err)
			}
		}
	}
}

type TunLoop struct {
	conn   net.Conn
	stream chan FromConn
	id     uint64
	ctx    map[uint64]net.Conn
	tunnel *TunConn
	router *Router

	diff  uint64
	mutex sync.RWMutex
	UniID uint64
}

func NewTunLoop(conn net.Conn, stream chan FromConn, router *Router) *TunLoop {
	loop := &TunLoop{}
	loop.conn = conn
	loop.stream = stream
	loop.ctx = map[uint64]net.Conn{}
	loop.tunnel = NewTunConn(loop.conn, 0)
	loop.router = router

	s1 := conn.LocalAddr().String()
	s2 := conn.RemoteAddr().String()
	if len(s1) > len(s2) {
		loop.diff = 0
	} else if len(s1) < len(s2) {
		loop.diff = uint64(s2[len(s1)])
	} else {
		for i := 0; i < len(s1); i++ {
			if s1[i] != s2[i] {
				loop.diff = uint64(s1[i])
				break
			}
		}
	}

	loop.diff = loop.diff << 32
	loop.id = loop.diff
	loop.UniID = EngineID()

	return loop
}

func (t *TunLoop) RemoteAddr() net.Addr {
	return t.conn.RemoteAddr()
}

func (t *TunLoop) Connect(loc Location) (net.Conn, error) {
	// TODO: forbid newID out of bound
	newID := atomic.AddUint64(&t.id, 1)
	if newID == 0 {
		newID = atomic.AddUint64(&t.id, 1)
	}

	if (newID & 0xFFFFFFFF) > 0xFFFFFFF {
		t.id = t.diff
		newID = atomic.AddUint64(&t.id, 1)
	}

	ch := NewChannelConn()
	tu := t.tunnel.Clone()
	tu.SetID(newID)

	var pkg Package
	pkg.Command = PkgCommandConnect
	pkg.Id = newID
	pkg.Loc = &loc

	err := t.tunnel.WritePackage(&pkg)
	if err != nil {
		log.Println("Redirect Fail:", loc.String())
		t.tunnel.Close()
		return nil, err
	}

	t.mutex.Lock()
	t.ctx[newID] = ch
	t.mutex.Unlock()

	conn := NewPipeConn(ch, tu)

	return conn, nil
}

func (t *TunLoop) Register(item RouterItem) {
	var pkg Package
	pkg.Command = PkgCommandRegister
	pkg.Router = &item

	t.tunnel.WritePackage(&pkg)
}

func (t *TunLoop) Run() {
	for {
		var pkg Package
		err := t.tunnel.ReadPackage(&pkg)
		if err != nil {
			log.Println("Read Over:", err)
			t.router.RemoveRouter(t.UniID)
			break
		}

		switch pkg.Command {
		case PkgCommandConnect:
			// new connection
			var from FromConn
			if pkg.Loc == nil {
				continue
			}

			from.Loc = *pkg.Loc

			ch := NewChannelConn()
			tu := t.tunnel.Clone()
			tu.SetID(pkg.Id)

			t.mutex.Lock()
			t.ctx[pkg.Id] = ch
			t.mutex.Unlock()
			from.Conn = NewPipeConn(ch, tu)

			t.stream <- from
		case PkgCommandData:
			t.mutex.RLock()
			to, present := t.ctx[pkg.Id]
			t.mutex.RUnlock()

			if !present {
				continue
			}

			// log.Println("Data", len(pkg.Data))

			_, err := to.Write(pkg.Data)
			if err != nil {
				t.mutex.Lock()
				delete(t.ctx, pkg.Id)
				t.mutex.Unlock()
				continue
			}
		case PkgCommandRegister:
			if pkg.Router == nil || pkg.Router.Domains == nil {
				log.Println("Register: router is nil")
				continue
			}

			var item RouterItem
			item.Domains = pkg.Router.Domains
			item.network = NewTunNetwork(t)
			t.router.AddRouter(item)
			// log.Println("Add Client Router:", item)
		case PkgCommandClose:
			t.mutex.RLock()
			ch, present := t.ctx[pkg.Id]
			t.mutex.RUnlock()
			if !present {
				continue
			}
			ch.Close()
			t.mutex.Lock()
			delete(t.ctx, pkg.Id)
			t.mutex.Unlock()
		}
	}
}

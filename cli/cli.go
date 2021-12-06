package cli

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/anycable/anycable-go/apollo"
	"github.com/anycable/anycable-go/config"
	"github.com/anycable/anycable-go/encoders"
	"github.com/anycable/anycable-go/identity"
	"github.com/anycable/anycable-go/metrics"
	"github.com/anycable/anycable-go/mrb"
	"github.com/anycable/anycable-go/netpoll"
	"github.com/anycable/anycable-go/node"
	"github.com/anycable/anycable-go/pubsub"
	"github.com/anycable/anycable-go/rails"
	"github.com/anycable/anycable-go/router"
	"github.com/anycable/anycable-go/rpc"
	"github.com/anycable/anycable-go/server"
	"github.com/anycable/anycable-go/utils"
	"github.com/anycable/anycable-go/version"
	"github.com/anycable/anycable-go/ws"
	"github.com/anycable/anycable-go/wspc"
	"github.com/apex/log"
	"github.com/gorilla/websocket"
	"github.com/syossan27/tebata"
)

type controllerFactory = func(*metrics.Metrics, *config.Config) (node.Controller, error)
type disconnectorFactory = func(*node.Node, *config.Config) (node.Disconnector, error)
type subscriberFactory = func(pubsub.Handler, *config.Config) (pubsub.Subscriber, error)
type websocketHandler = func(*node.Node, *config.Config) (http.Handler, error)

type Shutdownable interface {
	Shutdown() error
}

type Runner struct {
	name                string
	config              *config.Config
	controllerFactory   controllerFactory
	disconnectorFactory disconnectorFactory
	subscriberFactory   subscriberFactory
	websocketHandler    websocketHandler

	poller netpoll.Poller
	router *router.RouterController

	errChan       chan error
	shutdownables []Shutdownable
}

func NewRunner(name string, config *config.Config) *Runner {
	if name == "" {
		name = "AnyCable"
	}

	if config == nil {
		c, err := Config(os.Args[1:])

		if err != nil {
			panic(err)
		}

		config = &c
	}

	// Set global HTTP params as early as possible to make sure all servers use them
	server.SSL = &config.SSL
	server.Host = config.Host
	server.MaxConn = config.MaxConn

	return &Runner{name: name, config: config, shutdownables: []Shutdownable{}, errChan: make(chan error)}
}

func (r *Runner) ControllerFactory(fn controllerFactory) {
	r.controllerFactory = fn
}

func (r *Runner) DisconnectorFactory(fn disconnectorFactory) {
	r.disconnectorFactory = fn
}

func (r *Runner) SubscriberFactory(fn subscriberFactory) {
	r.subscriberFactory = fn
}

func (r *Runner) WebsocketHandler(fn websocketHandler) {
	r.websocketHandler = fn
}

func (r *Runner) Run() error {
	if ShowVersion() {
		fmt.Println(version.Version())
		return nil
	}

	if ShowHelp() {
		PrintHelp()
		return nil
	}

	config := r.config

	// init logging
	err := utils.InitLogger(config.LogFormat, config.LogLevel)

	if err != nil {
		return fmt.Errorf("!!! Failed to initialize logger !!!\n%v", err)
	}

	ctx := log.WithFields(log.Fields{"context": "main"})

	if DebugMode() {
		ctx.Debug("🔧 🔧 🔧 Debug mode is on 🔧 🔧 🔧")
	}

	mrubySupport := r.initMRuby()

	useNetpoll := false

	if config.App.NetpollEnabled {
		if config.SSL.Available() {
			return fmt.Errorf("!!! Using netpoll together with TLS is not supported yet !!!\n")
		}

		if config.WS.EnableCompression {
			ctx.Warn("WebSocket per message compression is not compatible with the current netpoll implementation. Disabling the compression.")
			config.WS.EnableCompression = false
		}

		poll, perr := netpoll.New(nil)

		if perr != nil {
			return fmt.Errorf("!!! Failed to initialize netpoll !!!\n%v", perr)
		}

		r.poller = poll
		useNetpoll = true
	}

	ctx.Infof("Starting %s %s%s (pid: %d, open file limit: %s, netpoll: %v)", r.name, version.Version(), mrubySupport, os.Getpid(), utils.OpenFileLimit(), useNetpoll)

	metrics, err := r.initMetrics(&config.Metrics)

	if err != nil {
		return fmt.Errorf("!!! Failed to initialize metrics writer !!!\n%v", err)
	}

	r.shutdownables = append(r.shutdownables, metrics)

	if config.WSPC.Enabled() {
		wspcServer, werr := r.initWSPC(metrics, config)

		if werr != nil {
			return fmt.Errorf("!!! Failed to initialize WS RPC server !!!\n%v", werr)
		}

		go func() {
			if wspcErr := wspcServer.Start(); wspcErr != nil {
				r.errChan <- fmt.Errorf("!!! WS RPC server failed to start !!!\n%v", wspcErr)
			}
		}()
	}

	controller, err := r.initController(metrics, config)

	if err != nil {
		return fmt.Errorf("!!! Failed to initialize controller !!!\n%v", err)
	}

	if config.JWT.Enabled() {
		identifier := identity.NewJWTIdentifier(&config.JWT)
		controller = identity.NewIdentifiableController(controller, identifier)
		ctx.Infof("JWT identification is enabled (param: %s, enforced: %v)", config.JWT.Param, config.JWT.Force)
	}

	if !r.Router().Empty() {
		r.Router().SetDefault(controller)
		controller = r.Router()
		ctx.Infof("Using channels router: %s", strings.Join(r.Router().Routes(), ", "))
	}

	appNode := node.NewNode(controller, metrics, &config.App)
	err = appNode.Start()

	if err != nil {
		return fmt.Errorf("!!! Failed to initialize application !!!\n%v", err)
	}

	disconnector, err := r.initDisconnector(appNode, config)

	if err != nil {
		return fmt.Errorf("!!! Failed to initialize disconnector !!!\n%v", err)
	}

	go disconnector.Run() // nolint:errcheck
	appNode.SetDisconnector(disconnector)

	subscriber, err := r.initSubscriber(appNode, config)

	if err != nil {
		return fmt.Errorf("Couldn't configure pub/sub: %v", err)
	}

	r.shutdownables = append(r.shutdownables, subscriber)

	go func() {
		if subscribeErr := subscriber.Start(); subscribeErr != nil {
			r.errChan <- fmt.Errorf("!!! Subscriber failed !!!\n%v", subscribeErr)
		}
	}()

	go func() {
		if contrErr := controller.Start(); contrErr != nil {
			r.errChan <- fmt.Errorf("!!! RPC failed !!!\n%v", contrErr)
		}
	}()

	wsServer, err := server.ForPort(strconv.Itoa(config.Port))
	if err != nil {
		return fmt.Errorf("!!! Failed to initialize WebSocket server at %s:%s !!!\n%v", err, config.Host, config.Port)
	}

	r.shutdownables = append(r.shutdownables, wsServer)

	wsHandler, err := r.initWebSocketHandler(appNode, config)
	if err != nil {
		return fmt.Errorf("!!! Failed to initialize WebSocket handler !!!\n%v", err)
	}

	wsServer.Mux.Handle(config.Path, wsHandler)

	ctx.Infof("Handle WebSocket connections at %s%s", wsServer.Address(), config.Path)

	if config.Apollo.Enabled() {
		gqlPath := config.Apollo.Path
		apolloHandler := r.apolloWebsocketHandler(appNode, config)

		wsServer.Mux.Handle(gqlPath, apolloHandler)

		ctx.Infof("Handle Apollo GraphQL WebSocket connections at %s%s", wsServer.Address(), gqlPath)
	}

	wsServer.Mux.Handle(config.HealthPath, http.HandlerFunc(server.HealthHandler))
	ctx.Infof("Handle health connections at %s%s", wsServer.Address(), config.HealthPath)

	go func() {
		if err = wsServer.StartAndAnnounce("WebSocket server"); err != nil {
			if !wsServer.Stopped() {
				r.errChan <- fmt.Errorf("WebSocket server at %s stopped: %v", wsServer.Address(), err)
			}
		}
	}()

	go func() {
		if err := metrics.Run(); err != nil {
			r.errChan <- fmt.Errorf("!!! Metrics module failed to start !!!\n%v", err)
		}
	}()

	r.shutdownables = append(r.shutdownables, appNode)

	r.announceGoPools()

	r.setupSignalHandlers()

	// Wait for an error (or none)
	return <-r.errChan
}

func (r *Runner) initMetrics(c *metrics.Config) (*metrics.Metrics, error) {
	m, err := metrics.FromConfig(c)

	if err != nil {
		return nil, err
	}

	if c.Statsd.Enabled() {
		sw := metrics.NewStatsdWriter(c.Statsd)
		m.RegisterWriter(sw)
	}

	return m, nil
}

func (r *Runner) initController(m *metrics.Metrics, c *config.Config) (node.Controller, error) {
	if r.controllerFactory == nil {
		return nil, errors.New("Controller factory is not specified")
	}

	return r.controllerFactory(m, c)
}

func (r *Runner) initDisconnector(n *node.Node, c *config.Config) (node.Disconnector, error) {
	if r.disconnectorFactory == nil {
		return r.defaultDisconnector(n, c)
	}

	return r.disconnectorFactory(n, c)
}

func (r *Runner) defaultDisconnector(n *node.Node, c *config.Config) (node.Disconnector, error) {
	if c.DisconnectorDisabled {
		return node.NewNoopDisconnector(), nil
	} else {
		return node.NewDisconnectQueue(n, &c.DisconnectQueue), nil
	}
}

func (r *Runner) initSubscriber(n *node.Node, c *config.Config) (pubsub.Subscriber, error) {
	if r.subscriberFactory == nil {
		return nil, errors.New("Subscriber factory is not specified")
	}

	return r.subscriberFactory(n, c)
}

func (r *Runner) initWebSocketHandler(n *node.Node, c *config.Config) (http.Handler, error) {
	if r.websocketHandler == nil {
		return r.defaultWebSocketHandler(n, c), nil
	}

	return r.websocketHandler(n, c)
}

func (r *Runner) defaultWebSocketHandler(n *node.Node, c *config.Config) http.Handler {
	return ws.WebsocketHandler(c.Headers, &c.WS, func(wsc *websocket.Conn, info *ws.RequestInfo, callback func()) error {
		wrappedConn := ws.NewConnection(wsc)
		session := node.NewSession(n, wrappedConn, info.Url, info.Headers, info.UID)

		if wsc.Subprotocol() == ws.ActionCableMsgpackProtocol {
			session.SetEncoder(encoders.Msgpack{})
		}

		if wsc.Subprotocol() == ws.ActionCableProtobufProtocol {
			session.SetEncoder(encoders.Protobuf{})
		}

		_, err := n.Authenticate(session)

		if err != nil {
			return err
		}

		if r.poller != nil {
			return session.ServeWithPoll(r.poller, callback)
		}

		return session.Serve(callback)
	})
}

func (r *Runner) apolloWebsocketHandler(n *node.Node, c *config.Config) http.Handler {
	return ws.WebsocketHandler(c.Headers, &c.WS, func(wsc *websocket.Conn, info *ws.RequestInfo, callback func()) error {
		wrappedConn := ws.NewConnection(wsc)

		session := node.NewSession(n, wrappedConn, info.Url, info.Headers, info.UID)
		session.SetEncoder(apollo.Encoder{})
		session.SetExecutor(apollo.NewExecutor(n, &c.Apollo))

		if r.poller != nil {
			return session.ServeWithPoll(r.poller, callback)
		}

		return session.Serve(callback)
	})
}

func (r *Runner) initMRuby() string {
	if mrb.Supported() {
		var mrbv string
		mrbv, err := mrb.Version()
		if err != nil {
			log.Errorf("mruby failed to initialize: %v", err)
		} else {
			return " (with " + mrbv + ")"
		}
	}

	return ""
}

func (r *Runner) Router() *router.RouterController {
	if r.router == nil {
		r.SetRouter(r.defaultRouter())
	}

	return r.router
}

func (r *Runner) SetRouter(router *router.RouterController) {
	r.router = router
}

func (r *Runner) defaultRouter() *router.RouterController {
	router := router.NewRouterController(nil)

	if r.config.Rails.TurboRailsKey != "" {
		turboController := rails.NewTurboController(r.config.Rails.TurboRailsKey)
		router.Route("Turbo::StreamsChannel", turboController) // nolint:errcheck
	}

	if r.config.Rails.CableReadyKey != "" {
		crController := rails.NewCableReadyController(r.config.Rails.CableReadyKey)
		router.Route("CableReady::Stream", crController) // nolint:errcheck
	}

	return router
}

func (r *Runner) announceGoPools() {
	configs := make([]string, 0)
	pools := utils.AllPools()

	for _, pool := range pools {
		configs = append(configs, fmt.Sprintf("%s: %d", pool.Name(), pool.Size()))
	}

	log.WithField("context", "main").Debugf("Go pools initialized (%s)", strings.Join(configs, ", "))
}

func (r *Runner) setupSignalHandlers() {
	t := tebata.New(syscall.SIGINT, syscall.SIGTERM)

	t.Reserve(func() { // nolint:errcheck
		log.Infof("Shutting down... (hit Ctrl-C to stop immediately)")
		go func() {
			termSig := make(chan os.Signal, 1)
			signal.Notify(termSig, syscall.SIGINT, syscall.SIGTERM)
			<-termSig
			log.Warnf("Immediate termination requested. Stopped")
			r.errChan <- nil
		}()
	})

	for _, shutdownable := range r.shutdownables {
		t.Reserve(shutdownable.Shutdown) // nolint:errcheck
	}

	t.Reserve(func() { r.errChan <- nil }) // nolint:errcheck
}

func (r *Runner) initWSPC(metrics *metrics.Metrics, config *config.Config) (*wspc.Server, error) {
	ws, err := wspc.NewServer(metrics, &config.WSPC)

	if err != nil {
		return nil, err
	}

	// IMPORTANT: Update RPC dialer to use this service
	config.RPC.DialFun = rpc.NewInprocessServiceDialer(ws, ws)

	return ws, nil
}

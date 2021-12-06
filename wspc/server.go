package wspc

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/anycable/anycable-go/metrics"
	pb "github.com/anycable/anycable-go/protos"
	"github.com/anycable/anycable-go/server"
	"github.com/apex/log"
)

type Server struct {
	metrics *metrics.Metrics
	config  *Config
	server  *server.HTTPServer
	log     *log.Entry
}

func NewServer(metrics *metrics.Metrics, config *Config) (*Server, error) {
	srv, err := server.ForPort(strconv.Itoa(config.Port))
	if err != nil {
		return nil, err
	}

	// srv.Mux.Handle(s.config.Path, http.HandlerFunc(handle))

	return &Server{
		metrics: metrics,
		config:  config,
		server:  srv,
		log:     log.WithField("context", "wspc"),
	}, nil
}

func (s *Server) Start() error {
	s.log.Infof("Handle RPC clients at %s%s", s.server.Address(), s.config.Path)

	if err := s.server.StartAndAnnounce("WS RPC server"); err != nil {
		if !s.server.Stopped() {
			return fmt.Errorf("WS RPC HTTP server at %s stopped: %v", s.server.Address(), err)
		}
	}

	return nil
}

// rpc.ClientHandler implementation

func (s *Server) Ready() error {
	return nil
}

func (s *Server) Close() {
	s.server.Shutdown() //nolint:errcheck
}

// pb.RPCServer implementation

func (s *Server) Connect(ctx context.Context, req *pb.ConnectionRequest) (*pb.ConnectionResponse, error) {
	return nil, errors.New("Not implemented")
}

func (s *Server) Command(ctx context.Context, req *pb.CommandMessage) (*pb.CommandResponse, error) {
	return nil, errors.New("Not implemented")
}

func (s *Server) Disconnect(ctx context.Context, req *pb.DisconnectRequest) (*pb.DisconnectResponse, error) {
	return nil, errors.New("Not implemented")
}

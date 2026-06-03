package registry

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"github.com/rydzu/ainfra/lb/pkg/pb"
)

type GRPCServer struct {
	pb.UnimplementedDiscoveryRegistryServer
	state *State
}

func NewGRPCServer(state *State) *GRPCServer {
	return &GRPCServer{state: state}
}

func (s *GRPCServer) RegisterService(ctx context.Context, req *pb.RegisterServiceRequest) (*pb.RegisterServiceResponse, error) {
	externalPort, err := s.state.Register(req)
	if err != nil {
		return nil, err
	}
	return &pb.RegisterServiceResponse{ExternalPort: externalPort}, nil
}

func (s *GRPCServer) DeregisterService(ctx context.Context, req *pb.DeregisterServiceRequest) (*pb.DeregisterServiceResponse, error) {
	removed, err := s.state.Deregister(req)
	if err != nil {
		return nil, err
	}
	return &pb.DeregisterServiceResponse{Removed: removed}, nil
}

func (s *GRPCServer) ListServices(ctx context.Context, req *pb.ListServicesRequest) (*pb.ListServicesResponse, error) {
	return s.state.ListResponse(), nil
}

func HTTPServicesHandler(state *State) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/services", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(state.Snapshot()); err != nil {
			log.Printf("encode /services response: %v", err)
		}
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

package k8sagent

import (
	"context"
	"testing"

	"github.com/rydzu/ainfra/lb/pkg/pb"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSelectPortName(t *testing.T) {
	multi := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "otel", Name: "collector"},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
			{Name: "otlp-grpc", Port: 4317},
			{Name: "otlp-http", Port: 4318},
			{Name: "health", Port: 13133},
		}},
	}
	single := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "monitoring", Name: "prometheus"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 9090}}},
	}
	unnamedSingle := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "monitoring", Name: "prometheus"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 9090}}},
	}

	tests := []struct {
		name         string
		svc          *corev1.Service
		externalPort int32
		want         string
	}{
		{"multi-port picks the matching port", multi, 4317, "otlp-grpc"},
		{"multi-port picks a different matching port", multi, 4318, "otlp-http"},
		{"multi-port without a match keeps every port", multi, 9999, ""},
		{"single port is always selected", single, 9091, "http"},
		{"unnamed single port has nothing to filter on", unnamedSingle, 0, ""},
		{"nil service", nil, 4317, ""},
		{"service without ports", &corev1.Service{}, 4317, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := selectPortName(tt.svc, tt.externalPort); got != tt.want {
				t.Fatalf("selectPortName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func multiPortEndpoints() *corev1.Endpoints {
	return &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Namespace: "otel", Name: "collector"},
		Subsets: []corev1.EndpointSubset{{
			Addresses: []corev1.EndpointAddress{{IP: "10.244.3.33"}},
			Ports: []corev1.EndpointPort{
				{Name: "otlp-grpc", Port: 4317},
				{Name: "otlp-http", Port: 4318},
				{Name: "health", Port: 13133},
			},
		}},
	}
}

func TestParseEndpointsRegistersOnlyTheSelectedPort(t *testing.T) {
	got := parseEndpoints(multiPortEndpoints(), intent{portName: "otlp-grpc"})
	if len(got) != 1 {
		t.Fatalf("expected 1 endpoint, got %d: %#v", len(got), got)
	}
	if _, ok := got["10.244.3.33:4317"]; !ok {
		t.Fatalf("expected the gRPC endpoint to be registered, got %#v", got)
	}
}

func TestParseEndpointsKeepsEveryPortWithoutASelection(t *testing.T) {
	got := parseEndpoints(multiPortEndpoints(), intent{})
	if len(got) != 3 {
		t.Fatalf("expected 3 endpoints, got %d: %#v", len(got), got)
	}
}

func TestParseEndpointsFallsBackWhenPortNameIsMissing(t *testing.T) {
	ep := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "manual"},
		Subsets: []corev1.EndpointSubset{{
			Addresses: []corev1.EndpointAddress{{IP: "10.0.0.1"}},
			Ports:     []corev1.EndpointPort{{Port: 8080}},
		}},
	}
	got := parseEndpoints(ep, intent{portName: "http"})
	if len(got) != 1 {
		t.Fatalf("expected the fallback to keep the single endpoint, got %#v", got)
	}
	if _, ok := got["10.0.0.1:8080"]; !ok {
		t.Fatalf("unexpected endpoint set %#v", got)
	}
}

func TestResolveIntentUsesServiceAnnotationsAndPort(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "otel",
			Name:      "collector",
			Annotations: map[string]string{
				annotationExpose:       "true",
				annotationServiceName:  "otel-edge",
				annotationExternalPort: "4317",
			},
		},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
			{Name: "otlp-grpc", Port: 4317},
			{Name: "otlp-http", Port: 4318},
			{Name: "health", Port: 13133},
		}},
	}
	agent := &Agent{client: fake.NewSimpleClientset(svc), known: map[string]map[string]Endpoint{}}

	in := agent.resolveIntent(multiPortEndpoints())
	if !in.exposed {
		t.Fatal("expected the Service annotation to expose the endpoints")
	}
	if in.account != "otel" {
		t.Fatalf("account = %q, want otel", in.account)
	}
	if in.serviceName != "otel-edge" {
		t.Fatalf("serviceName = %q, want otel-edge", in.serviceName)
	}
	if in.externalPort != 4317 {
		t.Fatalf("externalPort = %d, want 4317", in.externalPort)
	}
	if in.portName != "otlp-grpc" {
		t.Fatalf("portName = %q, want otlp-grpc", in.portName)
	}
}

func TestReconcileRegistersOnlyTheExposedPort(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "otel",
			Name:      "collector",
			Annotations: map[string]string{
				annotationExpose:       "true",
				annotationServiceName:  "otel-edge",
				annotationExternalPort: "4317",
			},
		},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
			{Name: "otlp-grpc", Port: 4317},
			{Name: "otlp-http", Port: 4318},
			{Name: "health", Port: 13133},
		}},
	}
	rpc := &recordingRegistry{}
	agent := &Agent{
		client: fake.NewSimpleClientset(svc),
		rpc:    rpc,
		known:  map[string]map[string]Endpoint{},
	}

	agent.reconcile(context.Background(), multiPortEndpoints())

	if len(rpc.registered) != 1 {
		t.Fatalf("expected exactly one registration, got %#v", rpc.registered)
	}
	req := rpc.registered[0]
	if req.TargetPort != 4317 {
		t.Fatalf("registered target port %d, want 4317", req.TargetPort)
	}
	if req.RequestedExternalPort != 4317 {
		t.Fatalf("requested external port %d, want 4317", req.RequestedExternalPort)
	}
	if req.ServiceName != "otel-edge" {
		t.Fatalf("service name %q, want otel-edge", req.ServiceName)
	}
}

type recordingRegistry struct {
	registered   []*pb.RegisterServiceRequest
	deregistered []*pb.DeregisterServiceRequest
}

func (r *recordingRegistry) RegisterService(_ context.Context, in *pb.RegisterServiceRequest, _ ...grpc.CallOption) (*pb.RegisterServiceResponse, error) {
	r.registered = append(r.registered, in)
	return &pb.RegisterServiceResponse{}, nil
}

func (r *recordingRegistry) DeregisterService(_ context.Context, in *pb.DeregisterServiceRequest, _ ...grpc.CallOption) (*pb.DeregisterServiceResponse, error) {
	r.deregistered = append(r.deregistered, in)
	return &pb.DeregisterServiceResponse{}, nil
}

func (r *recordingRegistry) ListServices(_ context.Context, _ *pb.ListServicesRequest, _ ...grpc.CallOption) (*pb.ListServicesResponse, error) {
	return &pb.ListServicesResponse{}, nil
}

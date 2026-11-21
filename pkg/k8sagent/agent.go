package k8sagent

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rydzu/ainfra/lb/pkg/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	annotationExpose       = "guardian.intent/expose"
	annotationAccount      = "guardian.intent/account"
	annotationServiceName  = "guardian.intent/service-name"
	annotationExternalPort = "guardian.intent/external-port"
)

type Endpoint struct {
	IP   string
	Port int32
}

// intent is the resolved LB registration intent for one Endpoints object.
type intent struct {
	account      string
	serviceName  string
	externalPort int32
	// portName is the single Endpoints port that backs externalPort. Empty
	// means "register every port" (single-port Services and manual Endpoints
	// without a resolvable Service).
	portName string
	exposed  bool
}

type Agent struct {
	client kubernetes.Interface
	rpc    pb.DiscoveryRegistryClient

	mu    sync.Mutex
	known map[string]map[string]Endpoint
}

func New(ctx context.Context, registryAddr string) (*Agent, error) {
	cfg, err := inClusterOrKubeConfig()
	if err != nil {
		return nil, err
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	conn, err := grpc.NewClient(registryAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("connect registry grpc %q: %w", registryAddr, err)
	}

	return &Agent{
		client: client,
		rpc:    pb.NewDiscoveryRegistryClient(conn),
		known:  make(map[string]map[string]Endpoint),
	}, nil
}

func (a *Agent) Run(ctx context.Context, namespace string) error {
	factory := informers.NewSharedInformerFactoryWithOptions(a.client, 30*time.Second,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = fields.Everything().String()
		}),
	)

	informer := factory.Core().V1().Endpoints().Informer()
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			a.reconcile(ctx, obj)
		},
		UpdateFunc: func(_, newObj interface{}) {
			a.reconcile(ctx, newObj)
		},
		DeleteFunc: func(obj interface{}) {
			a.handleDelete(ctx, obj)
		},
	})
	if err != nil {
		return err
	}

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return fmt.Errorf("k8s cache sync failed")
	}
	<-ctx.Done()
	return nil
}

func (a *Agent) reconcile(ctx context.Context, obj interface{}) {
	ep, ok := obj.(*corev1.Endpoints)
	if !ok {
		return
	}

	key := endpointResourceKey(ep.Namespace, ep.Name)
	in := a.resolveIntent(ep)
	if !in.exposed {
		a.deregisterAllRetryable(ctx, key, in.account, in.serviceName)
		return
	}

	current := parseEndpoints(ep, in)
	old := a.snapshotKnown(key)

	// Only track successfully-registered endpoints so failures retry on next resync.
	newKnown := make(map[string]Endpoint, len(current))

	for epKey, entry := range current {
		if _, exists := old[epKey]; exists {
			newKnown[epKey] = entry
			continue
		}
		_, err := a.rpc.RegisterService(ctx, &pb.RegisterServiceRequest{
			Account:               in.account,
			ServiceName:           in.serviceName,
			TargetIp:              entry.IP,
			TargetPort:            entry.Port,
			RequestedExternalPort: in.externalPort,
		})
		if err != nil {
			log.Printf("register %s/%s %s:%d failed: %v", ep.Namespace, in.serviceName, entry.IP, entry.Port, err)
			continue
		}
		newKnown[epKey] = entry
	}

	for epKey, entry := range old {
		if _, exists := current[epKey]; exists {
			continue
		}
		_, err := a.rpc.DeregisterService(ctx, &pb.DeregisterServiceRequest{
			Account:     in.account,
			ServiceName: in.serviceName,
			TargetIp:    entry.IP,
			TargetPort:  entry.Port,
		})
		if err != nil {
			log.Printf("deregister %s/%s %s:%d failed: %v", ep.Namespace, in.serviceName, entry.IP, entry.Port, err)
			newKnown[epKey] = entry
		}
	}

	a.setKnown(key, newKnown)
}

func (a *Agent) handleDelete(ctx context.Context, obj interface{}) {
	ep, ok := obj.(*corev1.Endpoints)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		ep, ok = tombstone.Obj.(*corev1.Endpoints)
		if !ok {
			return
		}
	}
	in := a.resolveIntent(ep)
	a.deregisterAll(ctx, endpointResourceKey(ep.Namespace, ep.Name), in.account, in.serviceName)
}

func (a *Agent) deregisterAll(ctx context.Context, key, account, serviceName string) {
	old := a.snapshotKnown(key)
	for _, entry := range old {
		_, err := a.rpc.DeregisterService(ctx, &pb.DeregisterServiceRequest{
			Account:     account,
			ServiceName: serviceName,
			TargetIp:    entry.IP,
			TargetPort:  entry.Port,
		})
		if err != nil {
			log.Printf("deregister %s/%s %s:%d failed: %v", account, serviceName, entry.IP, entry.Port, err)
		}
	}
	a.clearKnown(key)
}

func (a *Agent) deregisterAllRetryable(ctx context.Context, key, account, serviceName string) {
	old := a.snapshotKnown(key)
	remaining := make(map[string]Endpoint)
	for epKey, entry := range old {
		_, err := a.rpc.DeregisterService(ctx, &pb.DeregisterServiceRequest{
			Account:     account,
			ServiceName: serviceName,
			TargetIp:    entry.IP,
			TargetPort:  entry.Port,
		})
		if err != nil {
			log.Printf("deregister %s/%s %s:%d failed: %v", account, serviceName, entry.IP, entry.Port, err)
			remaining[epKey] = entry
		}
	}
	if len(remaining) == 0 {
		a.clearKnown(key)
	} else {
		a.setKnown(key, remaining)
	}
}

func (a *Agent) resolveIntent(ep *corev1.Endpoints) intent {
	anns := ep.GetAnnotations()
	in := intent{
		account:     anns[annotationAccount],
		serviceName: anns[annotationServiceName],
		exposed:     strings.EqualFold(anns[annotationExpose], "true"),
	}
	if in.account == "" {
		in.account = ep.Namespace
	}
	if in.serviceName == "" {
		in.serviceName = ep.Name
	}
	if raw := anns[annotationExternalPort]; raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			in.externalPort = int32(v)
		}
	}

	// The Service is needed both for annotation fallback and to learn which of
	// the Endpoints ports backs the exposed external port.
	svc, err := a.client.CoreV1().Services(ep.Namespace).Get(context.Background(), ep.Name, metav1.GetOptions{})
	if err != nil {
		return in
	}

	sanns := svc.GetAnnotations()
	if !in.exposed {
		if !strings.EqualFold(sanns[annotationExpose], "true") {
			return in
		}
		in.exposed = true
	}
	if in.account == ep.Namespace && sanns[annotationAccount] != "" {
		in.account = sanns[annotationAccount]
	}
	if in.serviceName == ep.Name && sanns[annotationServiceName] != "" {
		in.serviceName = sanns[annotationServiceName]
	}
	if in.externalPort == 0 && sanns[annotationExternalPort] != "" {
		if v, err := strconv.Atoi(sanns[annotationExternalPort]); err == nil {
			in.externalPort = int32(v)
		}
	}
	in.portName = selectPortName(svc, in.externalPort)
	return in
}

// selectPortName picks the single Service port that backs the pinned external
// port. Kubernetes Endpoints objects carry every port a Service defines, so
// without this selection one external port would round-robin across unrelated
// ports. The OTLP collector is the canonical case: it exposes gRPC 4317, HTTP
// 4318 and health 13133, and mixing them under external port 4317 makes gRPC
// exporters land on the HTTP receiver ("http2: frame too large").
//
// An empty result means "keep every port": the Service is unknown, declares no
// ports, or declares several ports without one matching the external port (an
// ambiguous intent, which we log but still serve the old way).
func selectPortName(svc *corev1.Service, externalPort int32) string {
	if svc == nil || len(svc.Spec.Ports) == 0 {
		return ""
	}
	ports := svc.Spec.Ports
	if externalPort != 0 {
		for _, p := range ports {
			if p.Port == externalPort {
				return p.Name
			}
		}
	}
	if len(ports) == 1 {
		return ports[0].Name
	}
	names := make([]string, 0, len(ports))
	for _, p := range ports {
		names = append(names, fmt.Sprintf("%s=%d", p.Name, p.Port))
	}
	log.Printf("service %s/%s exposes %d ports (%s) but pins external port %d to none of them; registering every endpoint port",
		svc.Namespace, svc.Name, len(ports), strings.Join(names, ","), externalPort)
	return ""
}

func parseEndpoints(ep *corev1.Endpoints, in intent) map[string]Endpoint {
	all := parseAllEndpoints(ep)
	if in.portName == "" || len(all) == 0 {
		return all
	}
	out := make(map[string]Endpoint, len(all))
	for _, subset := range ep.Subsets {
		for _, addr := range subset.Addresses {
			for _, port := range subset.Ports {
				if port.Name != in.portName {
					continue
				}
				e := Endpoint{IP: addr.IP, Port: port.Port}
				out[endpointKey(e.IP, e.Port)] = e
			}
		}
	}
	if len(out) == 0 {
		// Endpoint port names and Service port names are expected to match. If
		// they do not (for example a manually managed Endpoints object without
		// port names), fall back to every port instead of black-holing traffic.
		log.Printf("endpoints %s/%s have no port named %q; keeping all %d endpoint ports",
			ep.Namespace, ep.Name, in.portName, len(all))
		return all
	}
	return out
}

func parseAllEndpoints(ep *corev1.Endpoints) map[string]Endpoint {
	out := make(map[string]Endpoint)
	for _, subset := range ep.Subsets {
		for _, addr := range subset.Addresses {
			for _, port := range subset.Ports {
				e := Endpoint{IP: addr.IP, Port: port.Port}
				out[endpointKey(e.IP, e.Port)] = e
			}
		}
	}
	return out
}

func endpointKey(ip string, port int32) string {
	return ip + ":" + strconv.Itoa(int(port))
}

func endpointResourceKey(namespace, name string) string {
	return namespace + "/" + name
}

func (a *Agent) snapshotKnown(key string) map[string]Endpoint {
	a.mu.Lock()
	defer a.mu.Unlock()
	current := a.known[key]
	copyMap := make(map[string]Endpoint, len(current))
	for k, v := range current {
		copyMap[k] = v
	}
	return copyMap
}

func (a *Agent) setKnown(key string, entries map[string]Endpoint) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.known[key] = entries
}

func (a *Agent) clearKnown(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.known, key)
}

func inClusterOrKubeConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
}

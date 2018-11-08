package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"k8s.io/apimachinery/pkg/fields"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/api/compute/v1"
	"k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

const (
	uidConfigMapName = "ingress-uid"
	uidConfigMapKey  = "uid"

	defaultNamespaceConfigMapName = "ingress-default-namespace"
	defaultNamespaceConfigMapKey  = "namespace"
)

type Controller struct {
	computeService *compute.Service
	clientset      *kubernetes.Clientset

	uid     string
	project string

	nodesMutex sync.RWMutex
	nodes      []*v1.Node

	nodePortServicesMutex sync.RWMutex
	nodePortServices      []*v1.Service

	ingressesMutex sync.RWMutex
	ingresses      []*v1beta1.Ingress

	portUpdateMutex  sync.Mutex
	cancelPortUpdate func()
	prevPortState    string

	urlmapUpdateMutex  sync.Mutex
	cancelUrlmapUpdate func()

	defaultNamespaceMutex  sync.Mutex
	defaultNamespace string
}

func (c *Controller) getDefaultNamespace() string {
	c.defaultNamespaceMutex.Lock()
	defer c.defaultNamespaceMutex.Unlock()

	return c.defaultNamespace
}

func (c *Controller) setDefaultNamespace(ns string) {
	c.defaultNamespaceMutex.Lock()
	defer c.defaultNamespaceMutex.Unlock()

	c.defaultNamespace = ns
}

func getUID(clientset *kubernetes.Clientset) (string, error) {
	cm, err := clientset.CoreV1().ConfigMaps(systemNamespace).Get(uidConfigMapName, metav1.GetOptions{})
	if statusErr, ok := err.(*errors.StatusError); err != nil && (!ok || statusErr.ErrStatus.Code != 404) {
		return "", err
	}

	if cm != nil && cm.Data[uidConfigMapKey] != "" {
		return cm.Data[uidConfigMapKey], nil
	}

	uid := make([]byte, 8)
	rand.Read(uid)

	str := hex.EncodeToString(uid)
	if isKubernetesError(err, metav1.StatusReasonNotFound) {
		cm, err = clientset.CoreV1().ConfigMaps(systemNamespace).Create(&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: uidConfigMapName,
				Namespace: systemNamespace,
			},
			Data: map[string]string{uidConfigMapKey: str},
		})
		if err != nil {
			return "", err
		}

		return cm.Data[uidConfigMapKey], nil
	}

	if cm.Data == nil {
		cm.Data = map[string]string{uidConfigMapKey: str}
	} else {
		cm.Data[uidConfigMapKey] = str
	}

	cm, err = clientset.CoreV1().ConfigMaps(systemNamespace).Update(cm)
	if err != nil {
		return "", err
	}

	return cm.Data[uidConfigMapKey], nil
}

func getDefaultNamespace(clientset *kubernetes.Clientset) <-chan string {
	//cm, err := clientset.CoreV1().ConfigMaps(systemNamespace).Get(defaultNamespaceConfigMapName, metav1.GetOptions{})
	//if statusErr, ok := err.(*errors.StatusError); err != nil && (!ok || statusErr.ErrStatus.Code != 404) {
	//	return "", err
	//}

	out := make(chan string, 1)
	go func() {
		var lastResourceVersion string
		for {
			cmEvents, err := clientset.CoreV1().ConfigMaps(systemNamespace).Watch(metav1.ListOptions{
				FieldSelector: fields.OneTermEqualSelector("metadata.name", defaultNamespaceConfigMapName).String(),
				ResourceVersion: lastResourceVersion,
			})
			if err != nil {
				close(out)
				fmt.Println(err.Error())
				return
			}

			for event := range cmEvents.ResultChan() {
				switch event.Type {
				case watch.Added, watch.Modified:
					cm := event.Object.(*v1.ConfigMap)
					out <- cm.Data[defaultNamespaceConfigMapKey]
					lastResourceVersion = cm.ResourceVersion
				case watch.Deleted:
					out <- ""
				case watch.Error:
					status := event.Object.(*metav1.Status)
					fmt.Println(status.Reason + ":", status.Message)
				}
			}
		}
	}()

	return out
}

func NewController(clientset *kubernetes.Clientset, project string, hc *http.Client) (*Controller, error) {
	computeService, err := compute.New(hc)
	if err != nil {
		return nil, err
	}

	uid, err := getUID(clientset)
	if err != nil {
		return nil, err
	}

	return &Controller{
		computeService: computeService,
		clientset:      clientset,
		uid:            uid,
		project:        project,
	}, nil
}

func (c *Controller) instanceGroupName() string {
	return fmt.Sprintf("k8s-ig--%s", c.uid)
}

func (c *Controller) backendName(port int32) string {
	return fmt.Sprintf("k8s-be-%d--%s", port, c.uid)
}

func (c *Controller) urlMapName() string {
	return fmt.Sprintf("k8s-um--%s", c.uid)
}

func (c *Controller) portName(port int32) string {
	return fmt.Sprintf("port%d", port)
}

func (c *Controller) updateCachedNodes(eventType watch.EventType, node *v1.Node) {
	c.nodesMutex.Lock()
	defer c.nodesMutex.Unlock()

	switch eventType {
	case watch.Added:
		c.nodes = append(c.nodes, node)
	case watch.Deleted:
		for i, cachedNode := range c.nodes {
			if cachedNode.Namespace == node.Namespace && cachedNode.Name == node.Name {
				c.nodes = append(c.nodes[:i], c.nodes[i+1:]...)
				break
			}
		}
	case watch.Modified:
		for i, cachedNode := range c.nodes {
			if cachedNode.Namespace == node.Namespace && cachedNode.Name == node.Name {
				c.nodes[i] = node
				break
			}
		}
	}
}

func (c *Controller) nodeZones() []string {
	c.nodesMutex.Lock()
	defer c.nodesMutex.Unlock()

	zoneMap := make(map[string]interface{})
	for _, node := range c.nodes {
		zoneMap[node.Labels["failure-domain.beta.kubernetes.io/zone"]] = nil
	}

	zones := make([]string, 0, len(zoneMap))
	for zone := range zoneMap {
		zones = append(zones, zone)
	}

	return zones
}

func (c *Controller) handleNodeEvent(eventType watch.EventType, node *v1.Node) {
	c.updateCachedNodes(eventType, node)

	if eventType != watch.Added {
		return
	}

	zone, instanceName := node.Labels["failure-domain.beta.kubernetes.io/zone"], node.Labels["kubernetes.io/hostname"]

	backoff(func() error {
		_, err := c.computeService.InstanceGroups.AddInstances(c.project, zone, c.instanceGroupName(), &compute.InstanceGroupsAddInstancesRequest{
			Instances: []*compute.InstanceReference{
				{Instance: fmt.Sprintf("projects/%s/zones/%s/instances/%s", c.project, zone, instanceName)},
			},
		}).Do()
		if isComputeError(err, "memberAlreadyExists") {
			return nil
		}

		return err
	})
}

func (c *Controller) updateCachedNodePortServices(eventType watch.EventType, service *v1.Service) {
	c.nodePortServicesMutex.Lock()
	defer c.nodePortServicesMutex.Unlock()

	switch eventType {
	case watch.Added:
		if service.Spec.Type != v1.ServiceTypeNodePort {
			return
		}

		c.nodePortServices = append(c.nodePortServices, service)

	case watch.Deleted:
		if service.Spec.Type != v1.ServiceTypeNodePort {
			return
		}

		for i, cachedService := range c.nodePortServices {
			if cachedService.Namespace == service.Namespace && cachedService.Name == service.Name {
				c.nodePortServices = append(c.nodePortServices[:i], c.nodePortServices[i+1:]...)
				break
			}
		}

	case watch.Modified:
		for i, cachedService := range c.nodePortServices {
			if cachedService.Namespace == service.Namespace && cachedService.Name == service.Name {
				if service.Spec.Type != v1.ServiceTypeNodePort {
					c.nodePortServices = append(c.nodePortServices[:i], c.nodePortServices[i+1:]...)
				} else {
					c.nodePortServices[i] = service
				}
				break
			}
		}
	}

	go c.updateLoadBalancer()
}

func (c *Controller) handleServiceEvent(eventType watch.EventType, service *v1.Service) {
	c.updateCachedNodePortServices(eventType, service)

	if service.Spec.Type != v1.ServiceTypeNodePort {
		return
	}

	go c.updatePorts()
}

func (c *Controller) getServicePortByName(service *v1.Service, portName string) *v1.ServicePort {
	c.nodePortServicesMutex.RLock()
	defer c.nodePortServicesMutex.RUnlock()

	for i := range service.Spec.Ports {
		port := &service.Spec.Ports[i]
		if port.Name == portName {
			return port
		}
	}

	log.Println("named port not found..." + service.Name + ":" + portName)
	return nil
}

func (c *Controller) getServicePortByNumber(service *v1.Service, portNumber int32) *v1.ServicePort {
	c.nodePortServicesMutex.RLock()
	defer c.nodePortServicesMutex.RUnlock()

	for i := range service.Spec.Ports {
		port := &service.Spec.Ports[i]
		if port.Port == portNumber {
			return port
		}
	}

	log.Println("port not found..." + service.Name + ":" + strconv.Itoa(int(portNumber)))
	return nil
}

func (c *Controller) updateCachedIngresses(eventType watch.EventType, ingress *v1beta1.Ingress) {
	c.ingressesMutex.Lock()
	defer c.ingressesMutex.Unlock()

	switch eventType {
	case watch.Added:
		c.ingresses = append(c.ingresses, ingress)
	case watch.Deleted:
		for i, cachedIngress := range c.ingresses {
			if cachedIngress.Namespace == ingress.Namespace && cachedIngress.Name == ingress.Name {
				c.ingresses = append(c.ingresses[:i], c.ingresses[i+1:]...)
				break
			}
		}
	case watch.Modified:
		for i, cachedIngress := range c.ingresses {
			if cachedIngress.Namespace == ingress.Namespace && cachedIngress.Name == ingress.Name {
				c.ingresses[i] = ingress
				break
			}
		}
	}
}

func (c *Controller) handleIngressEvent(eventType watch.EventType, ingress *v1beta1.Ingress) {
	c.updateCachedIngresses(eventType, ingress)
	go c.updateLoadBalancer()
}

func (c *Controller) updatePorts() {
	var ctx context.Context
	func() {
		c.portUpdateMutex.Lock()
		defer c.portUpdateMutex.Unlock()

		if c.cancelPortUpdate != nil {
			c.cancelPortUpdate()
		}

		ctx, c.cancelPortUpdate = context.WithCancel(context.Background())
	}()

	defer func() {
		c.portUpdateMutex.Lock()
		defer c.portUpdateMutex.Unlock()

		c.cancelPortUpdate = nil
	}()

	select {
	case <-ctx.Done():
		return
	case <-time.After(100 * time.Millisecond):
	}

	portMap := make(map[int32]interface{})
	func() {
		c.nodePortServicesMutex.RLock()
		defer c.nodePortServicesMutex.RUnlock()

		for _, service := range c.nodePortServices {
			for _, port := range service.Spec.Ports {
				portMap[port.NodePort] = nil
			}
		}
	}()

	ports := make([]int32, 0, len(portMap))
	for port := range portMap {
		ports = append(ports, port)
	}

	sort.Slice(ports, func(i, j int) bool {
		return ports[i] < ports[j]
	})

	portStrs := make([]string, len(ports))
	for i, port := range ports {
		portStrs[i] = strconv.FormatInt(int64(port), 10)
	}

	portState := strings.Join(portStrs, "|")
	if c.prevPortState == portState {
		return
	}

	c.prevPortState = portState

	namedPorts := make([]*compute.NamedPort, 0, len(ports))
	for _, port := range ports {
		namedPorts = append(namedPorts, &compute.NamedPort{
			Name: c.portName(port),
			Port: int64(port),
		})
	}

	select {
	case <-ctx.Done():
		return
	default:
	}

	c.portUpdateMutex.Lock()
	defer c.portUpdateMutex.Unlock()

	zones := c.nodeZones()
	doneChans := make([]chan struct{}, len(zones))
	for i := range zones {
		zone := zones[i]
		doneChans[i] = backoff(func() error {
			_, err := c.computeService.InstanceGroups.Insert(c.project, zone, &compute.InstanceGroup{
				Name:       c.instanceGroupName(),
				NamedPorts: namedPorts,
			}).Do()
			if err == nil {
				return nil
			} else if err != nil && !isComputeError(err, "alreadyExists") {
				return err
			}

			_, err = c.computeService.InstanceGroups.SetNamedPorts(c.project, zone, c.instanceGroupName(), &compute.InstanceGroupsSetNamedPortsRequest{
				NamedPorts: namedPorts,
			}).Do()
			return err
		})
	}

	for _, c := range doneChans {
		<-c
	}
}

func (c *Controller) createOrUpdateBackendService(be *compute.BackendService) error {
	service, err := c.computeService.BackendServices.Get(c.project, be.Name).Do()
	if err != nil && !isComputeNotFound(err) {
		return err
	}

	if service != nil {
		service.Backends = be.Backends
		_, err = c.computeService.BackendServices.Patch(c.project, be.Name, &compute.BackendService{
			Backends: be.Backends,
		}).Do()
		if isComputeError(err, "resourceNotReady") {
			return ErrBackoffRetry
		}

		return err
	}

	_, err = c.computeService.HttpHealthChecks.Insert(c.project, &compute.HttpHealthCheck{
		Name:               be.Name,
		Description:        "Default kubernetes L7 Loadbalancing health check.",
		Port:               be.Port,
		RequestPath:        "/healthz",
		CheckIntervalSec:   60,
		TimeoutSec:         60,
		UnhealthyThreshold: 10,
		HealthyThreshold:   1,
	}).Do()
	if err != nil && !isComputeError(err, "alreadyExists") {
		return err
	}

	be.HealthChecks = []string{fmt.Sprintf("projects/%s/global/httpHealthChecks/%s", c.project, be.Name)}
	op, err := c.computeService.BackendServices.Insert(c.project, be).Do()
	if err != nil && !isComputeError(err, "alreadyExists") {
		return err
	}

	return c.waitForOperation(op)
}

func (c *Controller) findServiceByName(namespace, name string) *v1.Service {
	c.nodePortServicesMutex.Lock()
	defer c.nodePortServicesMutex.Unlock()

	for _, service := range c.nodePortServices {
		if service.Namespace == namespace && service.Name == name {
			return service
		}
	}

	return nil
}

func (c *Controller) defaultHTTPHandlerService() *v1.Service {
	if svc := c.findServiceByName(systemNamespace, defaultHTTPHandlerName); svc != nil {
		return svc
	}

	if _, err := c.clientset.AppsV1().Deployments(systemNamespace).Create(defaultHTTPHandlerDeployment()); err != nil && !isKubernetesError(err, metav1.StatusReasonAlreadyExists) {
		panic(err)
	}

	if _, err := c.clientset.CoreV1().Services(systemNamespace).Create(defaultHTTPHandlerService()); err != nil && !isKubernetesError(err, metav1.StatusReasonAlreadyExists) {
		panic(err)
	}

	return c.defaultHTTPHandlerService()
}

func (c *Controller) makeBackendServiceObject(service *v1.Service, port *v1.ServicePort) *compute.BackendService {
	portName := port.Name
	if portName == "" {
		portName = strconv.Itoa(int(port.Port))
	}

	return &compute.BackendService{
		LoadBalancingScheme: "EXTERNAL",
		Description:         fmt.Sprintf(`{"kubernetes.io/service-name":"%s/%s","kubernetes.io/service-port":"%s"}`, service.Namespace, service.Name, "http"),
		Name:                c.backendName(port.NodePort),
		PortName:            c.portName(port.NodePort),
		Port:                int64(port.NodePort),
		Protocol:            "HTTP",
		SessionAffinity:     "NONE",
		TimeoutSec:          30,
	}
}

func (c *Controller) defaultBackendService() *compute.BackendService {
	service := c.defaultHTTPHandlerService()
	return c.makeBackendServiceObject(service, &service.Spec.Ports[0])
}

func (c *Controller) createOrUpdateUrlMap(hostRules []*compute.HostRule, pathMatchers []*compute.PathMatcher, defaultBackendService string) error {
	umName := c.urlMapName()
	defaultBackendServiceFDN := c.backendServiceFDN(defaultBackendService)

	for _, pathMatcher := range pathMatchers {
		pathMatcher.DefaultService = defaultBackendServiceFDN
	}

	um, err := c.computeService.UrlMaps.Get(c.project, umName).Do()
	if isComputeNotFound(err) {
		<-backoff(func() error {
			op, err := c.computeService.UrlMaps.Insert(c.project, &compute.UrlMap{
				Name:           umName,
				HostRules:      hostRules,
				PathMatchers:   pathMatchers,
				DefaultService: defaultBackendServiceFDN,
			}).Do()
			if isComputeError(err, "resourceNotReady") {
				return ErrBackoffRetry
			} else if err != nil {
				return err
			}

			return c.waitForOperation(op)
		})
	} else if err != nil {
		return err
	} else {
		newHostRules := make([]*compute.HostRule, 0, len(hostRules))
		for _, hostRule := range um.HostRules {
			if !strings.HasPrefix(hostRule.PathMatcher, "k8s-") {
				newHostRules = append(newHostRules, hostRule)
			}
		}

		for _, hostRule := range hostRules {
			newHostRules = append(newHostRules, hostRule)
		}

		newPathMatchers := make([]*compute.PathMatcher, len(pathMatchers))
		oldPathMatchers := make(map[string]*compute.PathMatcher, len(um.HostRules))
		for _, pathMatcher := range um.PathMatchers {
			if strings.HasPrefix(pathMatcher.Name, "k8s-") {
				oldPathMatchers[pathMatcher.Name] = pathMatcher
			} else {
				newPathMatchers = append(newPathMatchers, pathMatcher)
			}
		}

		for _, pathMatcher := range pathMatchers {
			matcher, ok := oldPathMatchers[pathMatcher.Name]
			if ok {
				matcher.PathRules = pathMatcher.PathRules
			} else {
				matcher = pathMatcher
			}

			newPathMatchers = append(newPathMatchers, matcher)
		}

		um.HostRules = newHostRules
		um.PathMatchers = newPathMatchers

		<-backoff(func() error {
			op, err := c.computeService.UrlMaps.Update(c.project, um.Name, um).Do()
			if isComputeError(err, "resourceNotReady") {
				return ErrBackoffRetry
			} else if err != nil {
				return err
			}

			return c.waitForOperation(op)
		})
	}

	return nil
}

func (c *Controller) updateLoadBalancer() {
	var ctx context.Context
	func() {
		c.urlmapUpdateMutex.Lock()
		defer c.urlmapUpdateMutex.Unlock()

		if c.cancelUrlmapUpdate != nil {
			c.cancelUrlmapUpdate()
		}

		ctx, c.cancelUrlmapUpdate = context.WithCancel(context.Background())
	}()

	defer func() {
		c.urlmapUpdateMutex.Lock()
		defer c.urlmapUpdateMutex.Unlock()

		c.cancelUrlmapUpdate = nil
	}()

	select {
	case <-ctx.Done():
		return
	case <-time.After(1000 * time.Millisecond):
	}

	services := map[int32]*compute.BackendService{}
	hostRuleMap := map[string]*compute.HostRule{}
	pathMatcherMap := map[string]*compute.PathMatcher{}
	pathRules := map[string]*compute.PathRule{}
	hostKeyMap := map[string]string{}

	defaultNamespace := c.getDefaultNamespace()
	for _, ingress := range func() []*v1beta1.Ingress {
		c.ingressesMutex.RLock()
		defer c.ingressesMutex.RUnlock()

		return c.ingresses[:]
	}() {
		for _, rule := range ingress.Spec.Rules {
			for _, path := range rule.HTTP.Paths {
				service := c.findServiceByName(ingress.Namespace, path.Backend.ServiceName)
				if service == nil {
					continue
				}

				var servicePort *v1.ServicePort
				if path.Backend.ServicePort.Type == intstr.String {
					servicePort = c.getServicePortByName(service, path.Backend.ServicePort.StrVal)
				} else {
					servicePort = c.getServicePortByNumber(service, path.Backend.ServicePort.IntVal)
				}

				if servicePort == nil {
					continue
				}

				port := servicePort.NodePort
				matcherKey := ingress.Namespace + ":" + rule.Host
				pathKey := fmt.Sprintf("%s/%d/%s", ingress.Namespace, port, rule.Host)

				matcherName, ok := hostKeyMap[matcherKey]
				if !ok {
					matcherName = "k8s-matcher-" + strconv.Itoa(len(hostKeyMap))
					hostKeyMap[matcherKey] = matcherName
				}

				if _, ok := hostRuleMap[matcherName]; !ok {
					hosts := []string{fmt.Sprintf("%s.%s", ingress.Namespace, rule.Host)}
					if ingress.Namespace == defaultNamespace {
						hosts = append(hosts, rule.Host)
					}

					hostRuleMap[matcherName] = &compute.HostRule{
						PathMatcher: matcherName,
						Hosts:       hosts,
					}
				}

				backendName := c.backendName(port)
				pathMatcher, ok := pathMatcherMap[matcherName]
				if !ok {
					pathMatcher = &compute.PathMatcher{Name: matcherName}
					pathMatcherMap[matcherName] = pathMatcher
				}

				pathRule, ok := pathRules[pathKey]
				if !ok {
					pathRule = &compute.PathRule{Service: c.backendServiceFDN(backendName)}
					pathMatcher.PathRules = append(pathMatcher.PathRules, pathRule)
					pathRules[pathKey] = pathRule
				}

				pathRule.Paths = append(pathRule.Paths, path.Path)

				if _, ok := services[port]; ok {
					continue
				}

				services[port] = c.makeBackendServiceObject(service, servicePort)
			}
		}
	}

	select {
	case <-ctx.Done():
		return
	default:
	}

	c.urlmapUpdateMutex.Lock()
	defer c.urlmapUpdateMutex.Unlock()

	backends := func(ign string, zones []string) []*compute.Backend {
		backends := make([]*compute.Backend, len(zones))
		for i, zone := range zones {
			backends[i] = &compute.Backend{
				BalancingMode:      "RATE",
				MaxRatePerInstance: 1,
				CapacityScaler:     1,
				Group:              fmt.Sprintf("projects/%s/zones/%s/instanceGroups/%s", c.project, zone, ign),
			}
		}
		return backends
	}(c.instanceGroupName(), c.nodeZones())

	defaultBackendService := c.defaultBackendService()
	services[int32(defaultBackendService.Port)] = defaultBackendService

	doneChans := make([]chan struct{}, 0, len(services))
	for port := range services {
		backendService := services[port]
		backendService.Backends = backends
		doneChans = append(doneChans, backoff(func() error {
			return c.createOrUpdateBackendService(backendService)
		}))
	}

	for _, c := range doneChans {
		<-c
	}

	hostRules := make([]*compute.HostRule, 0, len(hostRuleMap))
	for _, hostRule := range hostRuleMap {
		hostRules = append(hostRules, hostRule)
	}

	pathMatchers := make([]*compute.PathMatcher, 0, len(pathMatcherMap))
	for _, pathMatcher := range pathMatcherMap {
		pathMatchers = append(pathMatchers, pathMatcher)
	}

	<-backoff(func() error {
		return c.createOrUpdateUrlMap(hostRules, pathMatchers, defaultBackendService.Name)
	})
}

func (c *Controller) Run(ctx context.Context) error {

	nodeEvents, err := c.clientset.CoreV1().Nodes().Watch(metav1.ListOptions{})
	if err != nil {
		return err
	}

	serviceEvents, err := c.clientset.CoreV1().Services("").Watch(metav1.ListOptions{})
	if err != nil {
		return err
	}

	ingressEvents, err := c.clientset.ExtensionsV1beta1().Ingresses("").Watch(metav1.ListOptions{})
	if err != nil {
		return err
	}

	var lastNodeResourceVersion, lastServiceResourceVersion, lastIngressResourceVersion string

	defaultNamespaceChan := getDefaultNamespace(c.clientset)

	for {
		if err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()

		case event, ok := <-nodeEvents.ResultChan():
			if !ok {
				nodeEvents, err = c.clientset.CoreV1().Nodes().Watch(metav1.ListOptions{ResourceVersion: lastNodeResourceVersion})
			} else if event.Type != "" && event.Object != nil {
				obj := event.Object.(*v1.Node)
				lastNodeResourceVersion = obj.ResourceVersion
				go c.handleNodeEvent(event.Type, obj)
			} else {
				log.Println("nil node event")
			}

		case event, ok := <-serviceEvents.ResultChan():
			if !ok {
				serviceEvents, err = c.clientset.CoreV1().Services("").Watch(metav1.ListOptions{ResourceVersion: lastServiceResourceVersion})
			} else if event.Type != "" && event.Object != nil {
				obj := event.Object.(*v1.Service)
				lastServiceResourceVersion = obj.ResourceVersion
				go c.handleServiceEvent(event.Type, event.Object.(*v1.Service))
			} else {
				log.Println("nil service event")
			}

		case event, ok := <-ingressEvents.ResultChan():
			if !ok {
				ingressEvents, err = c.clientset.ExtensionsV1beta1().Ingresses("").Watch(metav1.ListOptions{ResourceVersion: lastIngressResourceVersion})
			} else if event.Type != "" && event.Object != nil {
				obj := event.Object.(*v1beta1.Ingress)
				lastIngressResourceVersion = obj.ResourceVersion
				go c.handleIngressEvent(event.Type, obj)
			} else {
				log.Println("nil ingress event")
			}

		case newDefaultNamespace, ok := <-defaultNamespaceChan:
			if !ok {
				defaultNamespaceChan = getDefaultNamespace(c.clientset)
			} else {
				c.setDefaultNamespace(newDefaultNamespace)
				go c.updateLoadBalancer()
			}
		}
	}
}

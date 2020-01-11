package server

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/docker/libkv/store"
	"github.com/elotl/cloud-instance-provider/pkg/api"
	"github.com/elotl/cloud-instance-provider/pkg/api/validation"
	"github.com/elotl/cloud-instance-provider/pkg/certs"
	"github.com/elotl/cloud-instance-provider/pkg/etcd"
	"github.com/elotl/cloud-instance-provider/pkg/nodeclient"
	"github.com/elotl/cloud-instance-provider/pkg/server/cloud"
	"github.com/elotl/cloud-instance-provider/pkg/server/cloud/azure"
	"github.com/elotl/cloud-instance-provider/pkg/server/events"
	"github.com/elotl/cloud-instance-provider/pkg/server/nodemanager"
	"github.com/elotl/cloud-instance-provider/pkg/server/registry"
	"github.com/elotl/cloud-instance-provider/pkg/util"
	"github.com/elotl/cloud-instance-provider/pkg/util/cloudinitfile"
	"github.com/elotl/cloud-instance-provider/pkg/util/conmap"
	"github.com/elotl/cloud-instance-provider/pkg/util/instanceselector"
	"github.com/elotl/cloud-instance-provider/pkg/util/timeoutmap"
	"github.com/elotl/cloud-instance-provider/pkg/util/validation/field"
	"github.com/golang/glog"
	vkapi "github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/trace"
	"golang.org/x/net/context"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	stats "k8s.io/kubernetes/pkg/kubelet/apis/stats/v1alpha1"
)

const (
	// Provider configuration defaults.
	defaultCPUCapacity    = "20"
	defaultMemoryCapacity = "100Gi"
	defaultPodCapacity    = "20"

	// Values used in tracing as attribute keys.
	namespaceKey     = "namespace"
	nameKey          = "name"
	containerNameKey = "containerName"
)

type Controller interface {
	Start(quit <-chan struct{}, wg *sync.WaitGroup)
	Dump() []byte
}

type ProviderConfig struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	Pods   string `json:"pods,omitempty"`
}

// todo:change this name
type InstanceProvider struct {
	KV                map[string]registry.Registryer
	Encoder           api.MilpaCodec
	SystemQuit        <-chan struct{}
	SystemWaitGroup   *sync.WaitGroup
	Controllers       map[string]Controller
	ItzoClientFactory nodeclient.ItzoClientFactoryer
	cloudClient       cloud.CloudClient
	controllerManager *ControllerManager
	//
	nodeName           string
	operatingSystem    string
	internalIP         string
	daemonEndpointPort int32
	config             ProviderConfig
	startTime          time.Time
	notifier           func(*v1.Pod)
}

const (
	milpaElectionDir      = "milpa/election"
	etcdClusterRegionPath = "milpa/cluster/region"
)

var (
	MaxEventListSize = 4000
)

func validateWriteToEtcd(client *etcd.SimpleEtcd) error {
	glog.Info("Validating write access to etcd (will block until we can connect)")
	wo := &store.WriteOptions{
		IsDir: false,
		TTL:   2 * time.Second,
	}

	err := client.PutNoTimeout("/milpa/startup", []byte("OK"), wo)
	if err != nil {
		return err
	}
	glog.Info("Write to etcd successful")
	return nil
}

func setupEtcd(configFile, dataDir string, quit <-chan struct{}, wg *sync.WaitGroup) (*etcd.SimpleEtcd, error) {
	// if we have client endpoints, don't start the server. This could
	// change in the future if we want the embedded server to join
	// existing etcd server, but, for now just don't start it.
	var client *etcd.SimpleEtcd
	glog.Infof("Starting Internal Etcd")
	etcdServer := etcd.EtcdServer{
		ConfigFile: configFile,
		DataDir:    dataDir,
	}
	err := etcdServer.Start(quit, wg)
	if err != nil {
		return nil, util.WrapError(
			err, "Error creating internal etcd storage backend")
	}
	client = etcdServer.Client
	err = validateWriteToEtcd(client)
	if err != nil {
		return nil, util.WrapError(err, "Fatal Error: Could not write to etcd")
	}
	return client, err
}

func ensureRegionUnchanged(etcdClient *etcd.SimpleEtcd, region string) error {
	glog.Infof("Ensuring region has not changed")
	var savedRegion string
	pair, err := etcdClient.Get(etcdClusterRegionPath)
	if err != nil {
		if err != store.ErrKeyNotFound {
			return err
		}
		_, _, err = etcdClient.AtomicPut(etcdClusterRegionPath, []byte(region), nil, nil)
		return err
	}
	savedRegion = string(pair.Value)
	if region != savedRegion {
		return fmt.Errorf(
			"Error: region has changed from %s to %s. "+
				"This is unsupported. "+
				"Please delete all cluster resources and rename your cluster.",
			savedRegion, region)
	}
	return nil
}

// InstanceProvider should implement node.PodLifecycleHandler
func NewInstanceProvider(nodeName, operatingSystem, internalIP, configFilePath string, daemonEndpointPort int32) (*InstanceProvider, error) {
	providerConfig, err := loadProviderConfig(configFilePath, nodeName)
	if err != nil {
		return nil, err
	}

	serverConfigFile, err := ParseConfig(configFilePath)
	if err != nil {
		glog.Errorf("Loading Config file (%s) failed with error: %s",
			configFilePath, err.Error())
		os.Exit(1)
	}
	errs := validateServerConfigFile(serverConfigFile)
	if len(errs) > 0 {
		return nil, fmt.Errorf("Invalid Server Config: %v", errs.ToAggregate())
	}

	// todo: systemQuit should get passed in...
	systemQuit, systemWG := SetupSignalHandler()
	etcdClient, err := setupEtcd(
		serverConfigFile.Etcd.Internal.ConfigFile,
		serverConfigFile.Etcd.Internal.DataDir,
		systemQuit,
		systemWG,
	)
	if err != nil {
		glog.Errorf("Etcd error: %s", err)
		os.Exit(1)
	}
	controllerID, err := getControllerID(etcdClient)
	if err != nil {
		glog.Errorf("Controller ID error: %s", err)
		os.Exit(1)
	}
	if serverConfigFile.Testing.ControllerID != "" {
		controllerID = serverConfigFile.Testing.ControllerID
	}
	nametag := serverConfigFile.Nodes.Nametag
	if nametag == "" {
		nametag = controllerID
	}

	glog.Infof("ControllerID: %s", controllerID)

	certFactory, err := certs.New(etcdClient)
	if err != nil {
		glog.Errorf("Error setting up certificate factory: %v", err)
		os.Exit(1)
	}

	cloudClient, err := ConfigureCloud(serverConfigFile, controllerID, nametag)
	if err != nil {
		glog.Errorln("Error configuring cloud client", err)
		os.Exit(1)
	}
	cloudRegion := cloudClient.GetAttributes().Region
	err = ensureRegionUnchanged(etcdClient, cloudRegion)
	if err != nil {
		glog.Errorln("Error ensuring Milpa region is unchanged", err)
		os.Exit(1)
	}
	clientCert, err := certFactory.CreateClientCert()
	if err != nil {
		glog.Errorf("Error creating node client certificate: %v", err)
		os.Exit(1)
	}
	cloudStatus := cloudClient.CloudStatusKeeper()
	cloudStatus.Start()
	statefulValidator := validation.NewStatefulValidator(
		cloudStatus,
		cloudClient.GetAttributes().Provider,
		cloudClient.GetVPCCIDRs(),
	)
	err = instanceselector.Setup(
		cloudClient.GetAttributes().Provider,
		cloudRegion,
		serverConfigFile.Nodes.DefaultInstanceType)
	if err != nil {
		glog.Errorf("Error setting up instance selector %s", err)
		os.Exit(1)
	}
	// Ugly: need to do validation of this field after we have setup
	// the instanceselector
	errs = validation.ValidateInstanceType(serverConfigFile.Nodes.DefaultInstanceType, field.NewPath("nodes.defaultInstanceType"))
	if len(errs) > 0 {
		glog.Errorf("Error validating server.yml: %v", errs.ToAggregate())
		os.Exit(1)
	}

	glog.Infof("Setting up events")
	eventSystem := events.NewEventSystem(systemQuit, systemWG)

	glog.Infof("Setting up registry")
	podRegistry := registry.NewPodRegistry(
		etcdClient, api.VersioningCodec{}, eventSystem, statefulValidator)
	nodeRegistry := registry.NewNodeRegistry(
		etcdClient, api.VersioningCodec{}, eventSystem)
	eventRegistry := registry.NewEventRegistry(
		etcdClient, api.VersioningCodec{}, eventSystem)
	logRegistry := registry.NewLogRegistry(
		etcdClient, api.VersioningCodec{}, eventSystem)
	metricsRegistry := registry.NewMetricsRegistry(240)
	kv := map[string]registry.Registryer{
		"Pod":    podRegistry,
		"Node":   nodeRegistry,
		"Event":  eventRegistry,
		"Log":    logRegistry,
		"Metric": metricsRegistry,
	}

	usePublicIPs := !cloudClient.ControllerInsideVPC()
	itzoClientFactory := nodeclient.NewItzoFactory(
		&certFactory.Root, *clientCert, usePublicIPs)
	nodeDispenser := nodemanager.NewNodeDispenser()
	podController := &PodController{
		podRegistry:       podRegistry,
		logRegistry:       logRegistry,
		metricsRegistry:   metricsRegistry,
		nodeLister:        nodeRegistry,
		nodeDispenser:     nodeDispenser,
		nodeClientFactory: itzoClientFactory,
		events:            eventSystem,
		cloudClient:       cloudClient,
		controllerID:      controllerID,
		nametag:           nametag,
		lastStatusReply:   conmap.NewStringTimeTime(),
	}
	imageIdCache := timeoutmap.New(false, nil)
	cloudInitFile := cloudinitfile.New(serverConfigFile.Nodes.CloudInitFile)
	fixedSizeVolume := cloudClient.GetAttributes().FixedSizeVolume
	nodeController := &nodemanager.NodeController{
		Config: nodemanager.NodeControllerConfig{
			PoolInterval:      7 * time.Second,
			HeartbeatInterval: 10 * time.Second,
			ReaperInterval:    10 * time.Second,
			ItzoVersion:       serverConfigFile.Nodes.Itzo.Version,
			ItzoURL:           serverConfigFile.Nodes.Itzo.URL,
		},
		NodeRegistry:  nodeRegistry,
		LogRegistry:   logRegistry,
		PodReader:     podRegistry,
		NodeDispenser: nodeDispenser,
		NodeScaler: nodemanager.NewBindingNodeScaler(
			nodeRegistry,
			serverConfigFile.Nodes.StandbyNodes,
			cloudStatus,
			serverConfigFile.Nodes.DefaultVolumeSize,
			fixedSizeVolume,
		),
		CloudClient:        cloudClient,
		NodeClientFactory:  itzoClientFactory,
		Events:             eventSystem,
		ImageIdCache:       imageIdCache,
		CloudInitFile:      cloudInitFile,
		CertificateFactory: certFactory,
		CloudStatus:        cloudStatus,
		BootImageTags:      serverConfigFile.Nodes.BootImageTags,
	}
	garbageController := &GarbageController{
		config: GarbageControllerConfig{
			CleanInstancesInterval:  60 * time.Second,
			CleanTerminatedInterval: 10 * time.Second,
		},
		podRegistry:  podRegistry,
		nodeRegistry: nodeRegistry,
		cloudClient:  cloudClient,
		controllerID: controllerID,
	}
	metricsController := &MetricsController{
		metricsRegistry: metricsRegistry,
		podLister:       podRegistry,
	}
	controllers := map[string]Controller{
		"PodController":     podController,
		"NodeController":    nodeController,
		"GarbageController": garbageController,
		"MetricsController": metricsController,
	}

	if azClient, ok := cloudClient.(*azure.AzureClient); ok {
		azureImageController := azure.NewImageController(
			controllerID, serverConfigFile.Nodes.BootImageTags, azClient)
		controllers["ImageController"] = azureImageController
	}
	controllerManager := NewControllerManager(controllers)

	s := &InstanceProvider{
		KV:                kv,
		Encoder:           api.VersioningCodec{},
		SystemQuit:        systemQuit,
		SystemWaitGroup:   systemWG,
		ItzoClientFactory: itzoClientFactory,
		cloudClient:       cloudClient,
		controllerManager: controllerManager,

		// Todo: cleanup these parameters after initial commit
		nodeName:           nodeName,
		operatingSystem:    operatingSystem,
		internalIP:         internalIP,
		daemonEndpointPort: daemonEndpointPort,
		config:             providerConfig,
		startTime:          time.Now(),
	}

	go controllerManager.Start()
	go controllerManager.WaitForShutdown(systemQuit, systemWG)

	controllerManager.StartControllers()

	if ctrl, ok := controllers["ImageController"]; ok {
		azureImageController := ctrl.(*azure.ImageController)
		glog.Infof("Downloading Milpa node image to local Azure subscription (this could take a few minutes)")
		azureImageController.WaitForAvailable()
	}

	err = validateBootImageTags(
		serverConfigFile.Nodes.BootImageTags, cloudClient)
	if err != nil {
		glog.Errorf("Failed to validate boot image tags.")
		os.Exit(1)
	}

	return s, err
}

func loadProviderConfig(providerConfig, nodeName string) (config ProviderConfig, err error) {
	data, err := ioutil.ReadFile(providerConfig)
	if err != nil {
		return config, fmt.Errorf("reading config %q: %v", providerConfig, err)
	}
	configMap := map[string]ProviderConfig{}
	err = json.Unmarshal(data, &configMap)
	if err != nil {
		log.G(context.TODO()).Errorf(
			"parsing config %q: %v", providerConfig, err)
		return config, err
	}
	if _, exist := configMap[nodeName]; exist {
		log.G(context.TODO()).Infof("found config for node %q", nodeName)
		config = configMap[nodeName]
	}
	if config.CPU == "" {
		config.CPU = defaultCPUCapacity
	}
	if config.Memory == "" {
		config.Memory = defaultMemoryCapacity
	}
	if config.Pods == "" {
		config.Pods = defaultPodCapacity
	}
	if _, err = resource.ParseQuantity(config.CPU); err != nil {
		return config, fmt.Errorf("Invalid CPU value %v", config.CPU)
	}
	if _, err = resource.ParseQuantity(config.Memory); err != nil {
		return config, fmt.Errorf("Invalid memory value %v", config.Memory)
	}
	if _, err = resource.ParseQuantity(config.Pods); err != nil {
		return config, fmt.Errorf("Invalid pods value %v", config.Pods)
	}
	return config, nil
}

func filterEventList(eventList *api.EventList) *api.EventList {
	if eventList != nil && len(eventList.Items) > MaxEventListSize {
		// Take the most recent MaxEventListSize items
		sort.Slice(eventList.Items, func(i, j int) bool {
			return eventList.Items[i].CreationTimestamp.Before(eventList.Items[j].CreationTimestamp)
		})
		size := len(eventList.Items)
		start := size - MaxEventListSize
		eventList.Items = eventList.Items[start:]
	}
	return eventList
}

func filterReplyObject(obj api.MilpaObject) api.MilpaObject {
	switch v := obj.(type) {
	case *api.EventList:
		return filterEventList(v)
	}
	return obj
}

func (p *InstanceProvider) CreatePod(ctx context.Context, pod *v1.Pod) error {
	ctx, span := trace.StartSpan(ctx, "CreatePod")
	defer span.End()
	ctx = addAttributes(ctx, span, namespaceKey, pod.Namespace, nameKey, pod.Name)
	log.G(ctx).Infof("CreatePod %q", pod.Name)
	//p.notifier(pod)
	return fmt.Errorf("not implemented")
}

func (p *InstanceProvider) UpdatePod(ctx context.Context, pod *v1.Pod) error {
	ctx, span := trace.StartSpan(ctx, "UpdatePod")
	defer span.End()
	ctx = addAttributes(ctx, span, namespaceKey, pod.Namespace, nameKey, pod.Name)
	log.G(ctx).Infof("UpdatePod %q", pod.Name)
	//p.notifier(pod)
	return fmt.Errorf("not implemented")
}

// DeletePod deletes the specified pod out of memory.
func (p *InstanceProvider) DeletePod(ctx context.Context, pod *v1.Pod) (err error) {
	ctx, span := trace.StartSpan(ctx, "DeletePod")
	defer span.End()
	ctx = addAttributes(ctx, span, namespaceKey, pod.Namespace, nameKey, pod.Name)
	log.G(ctx).Infof("DeletePod %q", pod.Name)
	//p.notifier(pod)
	return fmt.Errorf("not implemented")
}

func (p *InstanceProvider) GetPod(ctx context.Context, namespace, name string) (pod *v1.Pod, err error) {
	ctx, span := trace.StartSpan(ctx, "GetPod")
	defer span.End()
	ctx = addAttributes(ctx, span, namespaceKey, namespace, nameKey, name)
	log.G(ctx).Infof("GetPod %q", name)
	//p.notifier(pod)
	return nil, fmt.Errorf("not implemented")
}

func (p *InstanceProvider) GetContainerLogs(ctx context.Context, namespace, podName, containerName string, opts vkapi.ContainerLogOpts) (io.ReadCloser, error) {
	ctx, span := trace.StartSpan(ctx, "GetContainerLogs")
	defer span.End()
	ctx = addAttributes(ctx, span, namespaceKey, namespace, nameKey, podName, containerNameKey, containerName)
	log.G(ctx).Infof("GetContainerLogs %q", podName)
	//p.notifier(pod)
	return nil, fmt.Errorf("not implemented")
}

func (p *InstanceProvider) RunInContainer(ctx context.Context, namespace, podName, containerName string, cmd []string, attach vkapi.AttachIO) error {
	ctx, span := trace.StartSpan(ctx, "RunInContainer")
	defer span.End()
	ctx = addAttributes(ctx, span, namespaceKey, namespace, nameKey, podName, containerNameKey, containerName)
	log.G(ctx).Infof("RunInContainer %q %v", podName, cmd)
	//p.notifier(pod)
	return fmt.Errorf("not implemented")
}

func (p *InstanceProvider) GetPodStatus(ctx context.Context, namespace, name string) (*v1.PodStatus, error) {
	ctx, span := trace.StartSpan(ctx, "GetPodStatus")
	defer span.End()
	ctx = addAttributes(ctx, span, namespaceKey, namespace, nameKey, name)
	log.G(ctx).Infof("GetPodStatus %q", name)
	//p.notifier(pod)
	return nil, fmt.Errorf("not implemented")
}

// GetPods returns a list of all pods known to be "running".
func (p *InstanceProvider) GetPods(ctx context.Context) ([]*v1.Pod, error) {
	ctx, span := trace.StartSpan(ctx, "GetPods")
	defer span.End()
	log.G(ctx).Infof("GetPods")
	//p.notifier(pod)
	return nil, fmt.Errorf("not implemented")
}

func (p *InstanceProvider) ConfigureNode(ctx context.Context, n *v1.Node) {
	ctx, span := trace.StartSpan(ctx, "ConfigureNode")
	defer span.End()
	log.G(ctx).Infof("ConfigureNode")
	n.Status.Capacity = p.capacity()
	n.Status.Allocatable = p.capacity()
	n.Status.Conditions = p.nodeConditions()
	n.Status.Addresses = p.nodeAddresses()
	n.Status.DaemonEndpoints = p.nodeDaemonEndpoints()
	os := p.operatingSystem
	if os == "" {
		os = "Linux"
	}
	n.Status.NodeInfo.OperatingSystem = os
	n.Status.NodeInfo.Architecture = "amd64"
}

func (p *InstanceProvider) capacity() v1.ResourceList {
	return v1.ResourceList{
		"cpu":    resource.MustParse(p.config.CPU),
		"memory": resource.MustParse(p.config.Memory),
		"pods":   resource.MustParse(p.config.Pods),
	}
}

func (p *InstanceProvider) nodeConditions() []v1.NodeCondition {
	return []v1.NodeCondition{
		{
			Type:               "Ready",
			Status:             v1.ConditionTrue,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletReady",
			Message:            "kubelet is ready",
		},
		{
			Type:               "OutOfDisk",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasSufficientDisk",
			Message:            "kubelet has sufficient disk space available",
		},
		{
			Type:               "MemoryPressure",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasSufficientMemory",
			Message:            "kubelet has sufficient memory available",
		},
		{
			Type:               "DiskPressure",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasNoDiskPressure",
			Message:            "kubelet has no disk pressure",
		},
		{
			Type:               "NetworkUnavailable",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "RouteCreated",
			Message:            "RouteController created a route",
		},
	}
}

func (p *InstanceProvider) nodeAddresses() []v1.NodeAddress {
	return []v1.NodeAddress{
		{
			Type:    "InternalIP",
			Address: p.internalIP,
		},
	}
}

func (p *InstanceProvider) nodeDaemonEndpoints() v1.NodeDaemonEndpoints {
	return v1.NodeDaemonEndpoints{
		KubeletEndpoint: v1.DaemonEndpoint{
			Port: p.daemonEndpointPort,
		},
	}
}

func (p *InstanceProvider) GetStatsSummary(ctx context.Context) (*stats.Summary, error) {
	var span trace.Span
	ctx, span = trace.StartSpan(ctx, "GetStatsSummary")
	defer span.End()
	res := &stats.Summary{}
	res.Node = stats.NodeStats{
		NodeName:  p.nodeName,
		StartTime: metav1.NewTime(p.startTime),
	}
	//	time := metav1.NewTime(time.Now())
	//	for _, pod := range p.pods {
	//		var (
	//			totalUsageNanoCores uint64
	//			totalUsageBytes uint64
	//		)
	//		pss := stats.PodStats{
	//			PodRef: stats.PodReference{
	//				Name:      pod.Name,
	//				Namespace: pod.Namespace,
	//				UID:       string(pod.UID),
	//			},
	//			StartTime: pod.CreationTimestamp,
	//		}
	//		for _, container := range pod.Spec.Containers {
	//			dummyUsageNanoCores := uint64(rand.Uint32())
	//			totalUsageNanoCores += dummyUsageNanoCores
	//			dummyUsageBytes := uint64(rand.Uint32())
	//			totalUsageBytes += dummyUsageBytes
	//			pss.Containers = append(pss.Containers, stats.ContainerStats{
	//				Name:      container.Name,
	//				StartTime: pod.CreationTimestamp,
	//				CPU: &stats.CPUStats{
	//					Time:           time,
	//					UsageNanoCores: &dummyUsageNanoCores,
	//				},
	//				Memory: &stats.MemoryStats{
	//					Time:       time,
	//					UsageBytes: &dummyUsageBytes,
	//				},
	//			})
	//		}
	//		pss.CPU = &stats.CPUStats{
	//			Time:           time,
	//			UsageNanoCores: &totalUsageNanoCores,
	//		}
	//		pss.Memory = &stats.MemoryStats{
	//			Time:       time,
	//			UsageBytes: &totalUsageBytes,
	//		}
	//		res.Pods = append(res.Pods, pss)
	//	}
	return res, nil
}

// NotifyPods is called to set a pod notifier callback function. This should be
// called before any operations are done within the provider.
func (p *InstanceProvider) NotifyPods(ctx context.Context, notifier func(*v1.Pod)) {
	//p.notifier = notifier
}

func addAttributes(ctx context.Context, span trace.Span, attrs ...string) context.Context {
	if len(attrs)%2 == 1 {
		return ctx
	}
	for i := 0; i < len(attrs); i += 2 {
		ctx = span.WithField(ctx, attrs[i], attrs[i+1])
	}
	return ctx
}
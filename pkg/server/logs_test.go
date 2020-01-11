package server

import (
	"testing"

	"github.com/elotl/cloud-instance-provider/pkg/api"
	"github.com/elotl/cloud-instance-provider/pkg/nodeclient"
	"github.com/elotl/cloud-instance-provider/pkg/server/registry"
	"github.com/stretchr/testify/assert"
)

func setupLogTestServer() (InstanceProvider, func()) {
	nodeReg, closer1 := registry.SetupTestNodeRegistry()
	podReg, closer2 := registry.SetupTestPodRegistry()
	logReg, closer3 := registry.SetupTestLogRegistry()
	s := InstanceProvider{
		KV: map[string]registry.Registryer{
			"Pod":  podReg,
			"Node": nodeReg,
			"Log":  logReg,
		},
		ItzoClientFactory: nodeclient.NewMockItzoClientFactory(),
	}
	return s, func() { closer1(); closer2(); closer3() }
}

func TestGetLogForPod(t *testing.T) {
	// create a mock client that gets us data
	// assert it has the correct object reference and the
	// correct content
	s, closer := setupLogTestServer()
	defer closer()
	podReg := s.KV["Pod"].(*registry.PodRegistry)
	nodeReg := s.KV["Node"].(*registry.NodeRegistry)
	node := api.GetFakeNode()
	node.Status.Addresses = api.NewNetworkAddresses("1.2.3.4", "")
	_, err := nodeReg.CreateNode(node)

	assert.NoError(t, err)
	pod := api.GetFakePod()
	pod.Status.BoundNodeName = node.Name
	pod.Status.Phase = api.PodRunning
	_, err = podReg.CreatePod(pod)
	assert.NoError(t, err)

	logFile, err := s.findLog(pod.Name, "", 0, 0)
	assert.NoError(t, err)
	assert.Equal(t, "logs", logFile.Content)
	assert.Equal(t, "Pod", logFile.ParentObject.Kind)
	assert.Equal(t, pod.UID, logFile.ParentObject.UID)
}

func TestGetLogFromRegistry(t *testing.T) {
	s, closer := setupLogTestServer()
	defer closer()
	logInput := api.GetFakeLog()
	logReg := s.KV["Log"].(*registry.LogRegistry)
	_, err := logReg.CreateLog(logInput)
	assert.NoError(t, err)
	logFile, err := s.findLog(logInput.ParentObject.Name, logInput.Name, 0, 0)
	assert.NoError(t, err)
	assert.Equal(t, logInput.Content, logFile.Content)
	assert.Equal(t, "Node", logFile.ParentObject.Kind)
	assert.Equal(t, logInput.ParentObject.UID, logFile.ParentObject.UID)
}

func TestGetLogForNotRunningPod(t *testing.T) {
	s, closer := setupLogTestServer()
	defer closer()
	podReg := s.KV["Pod"].(*registry.PodRegistry)
	pod := api.GetFakePod()
	pod.Status.Phase = api.PodWaiting
	_, err := podReg.CreatePod(pod)
	assert.NoError(t, err)

	podLogData := "Old pod log lines"
	log := api.GetFakeLog()
	log.Content = podLogData
	log.ParentObject = api.ToObjectReference(pod)
	logReg := s.KV["Log"].(*registry.LogRegistry)
	_, err = logReg.CreateLog(log)
	assert.NoError(t, err)

	logFile, err := s.findLog(pod.Name, "", 0, 0)
	assert.NoError(t, err)
	assert.Equal(t, podLogData, logFile.Content)
}
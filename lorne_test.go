package main

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/flynn/flynn-host/types"
	"github.com/flynn/go-dockerclient"
	"github.com/technoweenie/grohl"
)

type nullLogger struct{}

func (nullLogger) Log(grohl.Data) error { return nil }

func init() { grohl.SetLogger(nullLogger{}) }

type dockerClient struct {
	createErr error
	startErr  error
	pullErr   error
	created   *docker.Config
	pulled    string
	started   bool
	hostConf  *docker.HostConfig
}

func (c *dockerClient) CreateContainer(config *docker.Config) (*docker.Container, error) {
	if c.createErr != nil {
		err := c.createErr
		c.createErr = nil
		return nil, err
	}
	c.created = config
	return &docker.Container{ID: "asdf"}, nil
}

func (c *dockerClient) StartContainer(id string, config *docker.HostConfig) error {
	if id != "asdf" {
		return errors.New("Invalid ID")
	}
	if c.startErr != nil {
		return c.startErr
	}
	c.started = true
	c.hostConf = config
	return nil
}

func (c *dockerClient) InspectContainer(id string) (*docker.Container, error) {
	container := &docker.Container{Volumes: make(map[string]string)}
	for v := range c.created.Volumes {
		container.Volumes[v] = "/var/lib/docker/vfs/dir/" + strings.Replace(v, "/", "-", -1)
	}
	return container, nil
}

func (c *dockerClient) PullImage(opts docker.PullImageOptions, w io.Writer) error {
	if c.pullErr != nil {
		return c.pullErr
	}
	c.pulled = opts.Repository
	return nil
}

func testProcess(job *host.Job, t *testing.T) (*State, *dockerClient) {
	client := &dockerClient{}
	return testProcessWithOpts(job, "", "", client, t), client
}

func testProcessWithOpts(job *host.Job, extAddr, bindAddr string, client *dockerClient, t *testing.T) *State {
	if client == nil {
		client = &dockerClient{}
	}
	state := processWithOpts(job, extAddr, bindAddr, client)

	if client.created != job.Config {
		t.Error("job not created")
	}
	if !client.started {
		t.Error("job not started")
	}
	sjob := state.GetJob("a")
	if sjob == nil || sjob.StartedAt.IsZero() || sjob.Status != host.StatusRunning || sjob.ContainerID != "asdf" {
		t.Error("incorrect state")
	}

	return state
}

func testProcessWithError(job *host.Job, client *dockerClient, err error, t *testing.T) *State {
	state := processWithOpts(job, "", "", client)

	sjob := state.GetJob(job.ID)
	if sjob.Status != host.StatusFailed {
		t.Errorf("expected status failed, got %s", sjob.Status)
	}
	if *sjob.Error != err.Error() {
		t.Error("error not saved")
	}
	return state
}

func processWithOpts(job *host.Job, extAddr, bindAddr string, client *dockerClient) *State {
	ports := make(chan int)
	state := NewState()
	go allocatePorts(ports, 500, 505)
	(&jobProcessor{
		externalAddr: extAddr,
		bindAddr:     bindAddr,
		docker:       client,
		state:        state,
		discoverd:    extAddr + ":1111",
	}).processJob(ports, job)
	return state
}

func TestProcessJob(t *testing.T) {
	testProcess(&host.Job{ID: "a", Config: &docker.Config{}}, t)
}

func TestProcessJobWithImplicitPorts(t *testing.T) {
	job := &host.Job{TCPPorts: 2, ID: "a", Config: &docker.Config{}}
	_, client := testProcess(job, t)

	if len(job.Config.Env) == 0 || !sliceHasString(job.Config.Env, "PORT=500") {
		t.Fatal("PORT env not set")
	}
	if !sliceHasString(job.Config.Env, "PORT_0=500") {
		t.Error("PORT_0 env not set")
	}
	if !sliceHasString(job.Config.Env, "PORT_1=501") {
		t.Error("PORT_1 env not set")
	}
	if _, ok := job.Config.ExposedPorts["500/tcp"]; !ok {
		t.Error("exposed port 500 not set")
	}
	if _, ok := job.Config.ExposedPorts["501/tcp"]; !ok {
		t.Error("exposed port 501 not set")
	}
	if b := client.hostConf.PortBindings["500/tcp"]; len(b) == 0 || b[0].HostPort != "500" {
		t.Error("port 500 binding not set")
	}
	if b := client.hostConf.PortBindings["501/tcp"]; len(b) == 0 || b[0].HostPort != "501" {
		t.Error("port 501 binding not set")
	}
}

func TestProcessWithImplicitPortsAndIP(t *testing.T) {
	job := &host.Job{ID: "a", TCPPorts: 2, Config: &docker.Config{}}
	client := &dockerClient{}
	testProcessWithOpts(job, "10.10.10.1", "127.0.42.1", client, t)

	b := client.hostConf.PortBindings["500/tcp"]
	if b[0].HostIp != "127.0.42.1" {
		t.Error("host ip not 127.0.42.1")
	}
	if len(b) == 0 || b[0].HostPort != "500" {
		t.Error("port 8080 binding not set")
	}
}

func TestProcessJobWithExplicitPorts(t *testing.T) {
	hostConfig := &docker.HostConfig{
		PortBindings: make(map[string][]docker.PortBinding, 2),
	}
	hostConfig.PortBindings["80/tcp"] = []docker.PortBinding{{HostPort: "8080"}}
	hostConfig.PortBindings["443/tcp"] = []docker.PortBinding{{HostPort: "8081"}}

	job := &host.Job{ID: "a", Config: &docker.Config{}, HostConfig: hostConfig}
	_, client := testProcess(job, t)

	if b := client.hostConf.PortBindings["80/tcp"]; len(b) == 0 || b[0].HostPort != "8080" {
		t.Error("port 8080 binding not set")
	}
	if b := client.hostConf.PortBindings["443/tcp"]; len(b) == 0 || b[0].HostPort != "8081" {
		t.Error("port 8081 binding not set")
	}
}

func TestProcessWithExtAddr(t *testing.T) {
	job := &host.Job{ID: "a", Config: &docker.Config{}}
	testProcessWithOpts(job, "10.10.10.1", "", nil, t)

	if !sliceHasString(job.Config.Env, "EXTERNAL_IP=10.10.10.1") {
		t.Error("EXTERNAL_IP not set")
	}
	if !sliceHasString(job.Config.Env, "DISCOVERD=10.10.10.1:1111") {
		t.Error("DISCOVERD not set")
	}
}

func TestProcessWithVolume(t *testing.T) {
	job := &host.Job{
		ID: "a",
		Config: &docker.Config{
			Volumes: map[string]struct{}{"/data": {}},
		},
	}
	state, _ := testProcess(job, t)
	j := state.GetJob("a")
	if j.Volumes["/data"] == "" {
		t.Error("Volume missing from state")
	}
}

func sliceHasString(slice []string, str string) bool {
	for _, s := range slice {
		if s == str {
			return true
		}
	}
	return false
}

func TestProcessWithPull(t *testing.T) {
	job := &host.Job{ID: "a", Config: &docker.Config{Image: "test/foo"}}
	client := &dockerClient{createErr: docker.ErrNoSuchImage}
	testProcessWithOpts(job, "", "", client, t)

	if client.pulled != "test/foo" {
		t.Error("image not pulled")
	}
}

func TestProcessWithCreateFailure(t *testing.T) {
	job := &host.Job{ID: "a", Config: &docker.Config{}}
	err := errors.New("undefined failure")
	client := &dockerClient{createErr: err}
	testProcessWithError(job, client, err, t)
}

func TestProcessWithPullFailure(t *testing.T) {
	job := &host.Job{ID: "a", Config: &docker.Config{}}
	err := errors.New("undefined failure")
	client := &dockerClient{createErr: docker.ErrNoSuchImage, pullErr: err}
	testProcessWithError(job, client, err, t)
}

func TestProcessWithStartFailure(t *testing.T) {
	job := &host.Job{ID: "a", Config: &docker.Config{}}
	err := errors.New("undefined failure")
	client := &dockerClient{startErr: err}
	testProcessWithError(job, client, err, t)
}

type schedulerSyncClient struct {
	removeErr error
	removed   []string
}

func (s *schedulerSyncClient) RemoveJobs(jobs []string) error {
	if s.removeErr != nil {
		return s.removeErr
	}
	s.removed = append(s.removed, jobs...)
	return nil
}

func TestSyncScheduler(t *testing.T) {
	events := make(chan host.Event)
	client := &schedulerSyncClient{}
	done := make(chan struct{})
	go func() {
		syncScheduler(client, events)
		close(done)
	}()

	events <- host.Event{Event: "stop", JobID: "a"}
	close(events)
	<-done

	if len(client.removed) != 1 && client.removed[0] != "a" {
		t.Error("job not removed")
	}
}

type streamClient struct {
	events chan *docker.Event
}

func (s *streamClient) Events() (*docker.EventStream, error) {
	return &docker.EventStream{Events: s.events}, nil
}

func (s *streamClient) InspectContainer(id string) (*docker.Container, error) {
	if id != "1" {
		return nil, errors.New("incorrect id")
	}
	return &docker.Container{State: docker.State{ExitCode: 1}}, nil
}

func TestStreamEvents(t *testing.T) {
	events := make(chan *docker.Event)
	state := NewState()
	state.AddJob(&host.Job{ID: "a"})
	state.SetContainerID("a", "1")
	state.SetStatusRunning("a", nil)

	done := make(chan struct{})
	go func() {
		streamEvents(&streamClient{events}, state)
		close(done)
	}()

	events <- &docker.Event{Status: "die", ID: "1"}
	close(events)
	<-done

	job := state.GetJob("a")
	if job.Status != host.StatusCrashed {
		t.Error("incorrect status")
	}
	if job.ExitCode != 1 {
		t.Error("incorrect exit code")
	}
}

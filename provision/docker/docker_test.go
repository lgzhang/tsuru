// Copyright 2015 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package docker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/fsouza/go-dockerclient"
	"github.com/fsouza/go-dockerclient/testing"
	"github.com/tsuru/config"
	"github.com/tsuru/docker-cluster/cluster"
	"github.com/tsuru/tsuru/api/apitest"
	"github.com/tsuru/tsuru/app"
	"github.com/tsuru/tsuru/db"
	"github.com/tsuru/tsuru/provision"
	"github.com/tsuru/tsuru/provision/provisiontest"
	"github.com/tsuru/tsuru/repository/repositorytest"
	"github.com/tsuru/tsuru/router/routertest"
	"github.com/tsuru/tsuru/safe"
	"gopkg.in/check.v1"
	"gopkg.in/mgo.v2/bson"
)

type newContainerOpts struct {
	AppName     string
	Status      string
	Provisioner *dockerProvisioner
}

func (s *S) newContainer(opts *newContainerOpts) (*container, error) {
	container := container{
		ID:       "id",
		IP:       "10.10.10.10",
		HostPort: "3333",
		HostAddr: "127.0.0.1",
	}
	p := s.p
	if opts != nil {
		container.Status = opts.Status
		container.AppName = opts.AppName
		if opts.Provisioner != nil {
			p = opts.Provisioner
		}
	}
	err := s.newFakeImage(p, "tsuru/python")
	if err != nil {
		return nil, err
	}
	if container.AppName == "" {
		container.AppName = "container"
	}
	routertest.FakeRouter.AddBackend(container.AppName)
	routertest.FakeRouter.AddRoute(container.AppName, container.getAddress())
	port, err := getPort()
	if err != nil {
		return nil, err
	}
	ports := map[docker.Port]struct{}{
		docker.Port(port + "/tcp"): {},
	}
	config := docker.Config{
		Image:        "tsuru/python",
		Cmd:          []string{"ps"},
		ExposedPorts: ports,
	}
	_, c, err := s.p.getCluster().CreateContainer(docker.CreateContainerOptions{Config: &config})
	if err != nil {
		return nil, err
	}
	container.ID = c.ID
	container.Image = "tsuru/python"
	conn, err := db.Conn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	err = conn.Collection(s.collName).Insert(&container)
	if err != nil {
		return nil, err
	}
	imageId, err := appCurrentImageName(container.AppName)
	if err != nil {
		return nil, err
	}
	err = s.newFakeImage(p, imageId)
	if err != nil {
		return nil, err
	}
	return &container, nil
}

func (s *S) removeTestContainer(c *container) error {
	routertest.FakeRouter.RemoveBackend(c.AppName)
	return c.remove(s.p)
}

func (s *S) newFakeImage(p *dockerProvisioner, repo string) error {
	var buf safe.Buffer
	opts := docker.PullImageOptions{Repository: repo, OutputStream: &buf}
	return p.getCluster().PullImage(opts, docker.AuthConfiguration{})
}

func (s *S) TestContainerGetAddress(c *check.C) {
	container := container{ID: "id123", HostAddr: "10.10.10.10", HostPort: "49153"}
	address := container.getAddress()
	expected := "http://10.10.10.10:49153"
	c.Assert(address, check.Equals, expected)
}

func (s *S) TestContainerCreate(c *check.C) {
	app := provisiontest.NewFakeApp("app-name", "brainfuck", 1)
	app.Memory = 15
	app.Swap = 15
	app.CpuShare = 50
	routertest.FakeRouter.AddBackend(app.GetName())
	defer routertest.FakeRouter.RemoveBackend(app.GetName())
	s.p.getCluster().PullImage(
		docker.PullImageOptions{Repository: "tsuru/brainfuck"},
		docker.AuthConfiguration{},
	)
	cont := container{Name: "myName", AppName: app.GetName(), Type: app.GetPlatform(), Status: "created"}
	err := cont.create(runContainerActionsArgs{
		app:         app,
		imageID:     s.p.getBuildImage(app),
		commands:    []string{"docker", "run"},
		provisioner: s.p,
	})
	c.Assert(err, check.IsNil)
	defer s.removeTestContainer(&cont)
	c.Assert(cont.ID, check.Not(check.Equals), "")
	c.Assert(cont, check.FitsTypeOf, container{})
	c.Assert(cont.AppName, check.Equals, app.GetName())
	c.Assert(cont.Type, check.Equals, app.GetPlatform())
	u, _ := url.Parse(s.server.URL())
	host, _, _ := net.SplitHostPort(u.Host)
	c.Assert(cont.HostAddr, check.Equals, host)
	user, err := config.GetString("docker:ssh:user")
	c.Assert(err, check.IsNil)
	c.Assert(cont.User, check.Equals, user)
	dcli, _ := docker.NewClient(s.server.URL())
	container, err := dcli.InspectContainer(cont.ID)
	c.Assert(err, check.IsNil)
	c.Assert(container.Path, check.Equals, "docker")
	c.Assert(container.Args, check.DeepEquals, []string{"run"})
	c.Assert(container.Config.User, check.Equals, user)
	c.Assert(container.Config.Memory, check.Equals, app.Memory)
	c.Assert(container.Config.MemorySwap, check.Equals, app.Memory+app.Swap)
	c.Assert(container.Config.CPUShares, check.Equals, int64(app.CpuShare))
}

func (s *S) TestContainerCreateAlocatesPort(c *check.C) {
	app := provisiontest.NewFakeApp("app-name", "brainfuck", 1)
	app.Memory = 15
	routertest.FakeRouter.AddBackend(app.GetName())
	defer routertest.FakeRouter.RemoveBackend(app.GetName())
	s.p.getCluster().PullImage(
		docker.PullImageOptions{Repository: "tsuru/brainfuck"},
		docker.AuthConfiguration{},
	)
	cont := container{Name: "myName", AppName: app.GetName(), Type: app.GetPlatform(), Status: "created"}
	err := cont.create(runContainerActionsArgs{
		app:         app,
		imageID:     s.p.getBuildImage(app),
		commands:    []string{"docker", "run"},
		provisioner: s.p,
	})
	c.Assert(err, check.IsNil)
	defer s.removeTestContainer(&cont)
	info, err := cont.networkInfo(s.p)
	c.Assert(err, check.IsNil)
	c.Assert(info.HTTPHostPort, check.Not(check.Equals), "")
}

func (s *S) TestContainerCreateDoesNotAlocatesPortForDeploy(c *check.C) {
	app := provisiontest.NewFakeApp("app-name", "brainfuck", 1)
	app.Memory = 15
	routertest.FakeRouter.AddBackend(app.GetName())
	defer routertest.FakeRouter.RemoveBackend(app.GetName())
	s.p.getCluster().PullImage(
		docker.PullImageOptions{Repository: "tsuru/brainfuck"},
		docker.AuthConfiguration{},
	)
	cont := container{Name: "myName", AppName: app.GetName(), Type: app.GetPlatform(), Status: "created"}
	err := cont.create(runContainerActionsArgs{
		isDeploy:    true,
		app:         app,
		imageID:     s.p.getBuildImage(app),
		commands:    []string{"docker", "run"},
		provisioner: s.p,
	})
	c.Assert(err, check.IsNil)
	defer s.removeTestContainer(&cont)
	info, err := cont.networkInfo(s.p)
	c.Assert(err, check.IsNil)
	c.Assert(info.HTTPHostPort, check.Equals, "")
}

func (s *S) TestContainerCreateUndefinedUser(c *check.C) {
	oldUser, _ := config.Get("docker:ssh:user")
	defer config.Set("docker:ssh:user", oldUser)
	config.Unset("docker:ssh:user")
	err := s.newFakeImage(s.p, "tsuru/python")
	c.Assert(err, check.IsNil)
	app := provisiontest.NewFakeApp("app-name", "python", 1)
	routertest.FakeRouter.AddBackend(app.GetName())
	defer routertest.FakeRouter.RemoveBackend(app.GetName())
	cont := container{Name: "myName", AppName: app.GetName(), Type: app.GetPlatform(), Status: "created"}
	err = cont.create(runContainerActionsArgs{
		app:         app,
		imageID:     s.p.getBuildImage(app),
		commands:    []string{"docker", "run"},
		provisioner: s.p,
	})
	c.Assert(err, check.IsNil)
	defer s.removeTestContainer(&cont)
	dcli, _ := docker.NewClient(s.server.URL())
	container, err := dcli.InspectContainer(cont.ID)
	c.Assert(err, check.IsNil)
	c.Assert(container.Config.User, check.Equals, "")
}

func (s *S) TestGetPort(c *check.C) {
	port, err := getPort()
	c.Assert(err, check.IsNil)
	c.Assert(port, check.Equals, s.port)
}

func (s *S) TestGetPortUndefined(c *check.C) {
	old, _ := config.Get("docker:run-cmd:port")
	defer config.Set("docker:run-cmd:port", old)
	config.Unset("docker:run-cmd:port")
	port, err := getPort()
	c.Assert(port, check.Equals, "")
	c.Assert(err, check.NotNil)
}

func (s *S) TestGetPortInteger(c *check.C) {
	old, _ := config.Get("docker:run-cmd:port")
	defer config.Set("docker:run-cmd:port", old)
	config.Set("docker:run-cmd:port", 8888)
	port, err := getPort()
	c.Assert(err, check.IsNil)
	c.Assert(port, check.Equals, "8888")
}

func (s *S) TestContainerSetStatus(c *check.C) {
	update := time.Date(1989, 2, 2, 14, 59, 32, 0, time.UTC).In(time.UTC)
	container := container{ID: "something-300", LastStatusUpdate: update}
	coll := s.p.collection()
	defer coll.Close()
	coll.Insert(container)
	defer coll.Remove(bson.M{"id": container.ID})
	container.setStatus(s.p, "what?!")
	c2, err := s.p.getContainer(container.ID)
	c.Assert(err, check.IsNil)
	c.Assert(c2.Status, check.Equals, "what?!")
	lastUpdate := c2.LastStatusUpdate.In(time.UTC).Format(time.RFC822)
	c.Assert(lastUpdate, check.Not(check.DeepEquals), update.Format(time.RFC822))
	c.Assert(c2.LastSuccessStatusUpdate.IsZero(), check.Equals, true)
}

func (s *S) TestContainerSetStatusStarted(c *check.C) {
	container := container{ID: "telnet"}
	coll := s.p.collection()
	defer coll.Close()
	err := coll.Insert(container)
	c.Assert(err, check.IsNil)
	defer coll.Remove(bson.M{"id": container.ID})
	container.setStatus(s.p, provision.StatusStarted.String())
	c2, err := s.p.getContainer(container.ID)
	c.Assert(err, check.IsNil)
	c.Assert(c2.Status, check.Equals, provision.StatusStarted.String())
	c.Assert(c2.LastSuccessStatusUpdate.IsZero(), check.Equals, false)
	c2.LastSuccessStatusUpdate = time.Time{}
	err = coll.Update(bson.M{"id": c2.ID}, c2)
	c.Assert(err, check.IsNil)
	c2.setStatus(s.p, provision.StatusStarting.String())
	c3, err := s.p.getContainer(container.ID)
	c.Assert(err, check.IsNil)
	c.Assert(c3.LastSuccessStatusUpdate.IsZero(), check.Equals, false)
}

func (s *S) TestContainerSetImage(c *check.C) {
	container := container{ID: "something-300"}
	coll := s.p.collection()
	defer coll.Close()
	coll.Insert(container)
	defer coll.Remove(bson.M{"id": container.ID})
	container.setImage(s.p, "newimage")
	c2, err := s.p.getContainer(container.ID)
	c.Assert(err, check.IsNil)
	c.Assert(c2.Image, check.Equals, "newimage")
}

func (s *S) TestContainerRemove(c *check.C) {
	conn, err := db.Conn()
	defer conn.Close()
	err = conn.Apps().Insert(app.App{Name: "test-app"})
	c.Assert(err, check.IsNil)
	defer conn.Apps().Remove(bson.M{"name": "test-app"})
	container, err := s.newContainer(&newContainerOpts{AppName: "test-app"})
	c.Assert(err, check.IsNil)
	defer s.removeTestContainer(container)
	err = container.remove(s.p)
	c.Assert(err, check.IsNil)
	coll := s.p.collection()
	defer coll.Close()
	err = coll.Find(bson.M{"id": container.ID}).One(&container)
	c.Assert(err, check.NotNil)
	c.Assert(err.Error(), check.Equals, "not found")
	c.Assert(routertest.FakeRouter.HasRoute(container.AppName, container.getAddress()), check.Equals, false)
	client, _ := docker.NewClient(s.server.URL())
	_, err = client.InspectContainer(container.ID)
	c.Assert(err, check.NotNil)
	_, ok := err.(*docker.NoSuchContainer)
	c.Assert(ok, check.Equals, true)
}

func (s *S) TestRemoveContainerIgnoreErrors(c *check.C) {
	conn, err := db.Conn()
	defer conn.Close()
	err = conn.Apps().Insert(app.App{Name: "test-app"})
	c.Assert(err, check.IsNil)
	defer conn.Apps().Remove(bson.M{"name": "test-app"})
	container, err := s.newContainer(&newContainerOpts{AppName: "test-app"})
	c.Assert(err, check.IsNil)
	defer s.removeTestContainer(container)
	client, _ := docker.NewClient(s.server.URL())
	err = client.RemoveContainer(docker.RemoveContainerOptions{ID: container.ID})
	c.Assert(err, check.IsNil)
	err = container.remove(s.p)
	c.Assert(err, check.IsNil)
	coll := s.p.collection()
	defer coll.Close()
	err = coll.Find(bson.M{"id": container.ID}).One(&container)
	c.Assert(err, check.NotNil)
	c.Assert(err.Error(), check.Equals, "not found")
	c.Assert(routertest.FakeRouter.HasRoute(container.AppName, container.getAddress()), check.Equals, false)
}

func (s *S) TestContainerNetworkInfo(c *check.C) {
	cont, err := s.newContainer(nil)
	c.Assert(err, check.IsNil)
	defer s.removeTestContainer(cont)
	info, err := cont.networkInfo(s.p)
	c.Assert(err, check.IsNil)
	c.Assert(info.IP, check.Not(check.Equals), "")
	c.Assert(info.HTTPHostPort, check.Not(check.Equals), "")
}

func (s *S) TestContainerNetworkInfoNotFound(c *check.C) {
	inspectOut := `{
	"NetworkSettings": {
		"IpAddress": "10.10.10.10",
		"IpPrefixLen": 8,
		"Gateway": "10.65.41.1",
		"Ports": {}
	}
}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/containers/") {
			w.Write([]byte(inspectOut))
		}
	}))
	defer server.Close()
	var storage cluster.MapStorage
	storage.StoreContainer("c-01", server.URL)
	var p dockerProvisioner
	var err error
	p.cluster, err = cluster.New(nil, &storage,
		cluster.Node{Address: server.URL},
	)
	c.Assert(err, check.IsNil)
	container := container{ID: "c-01"}
	info, err := container.networkInfo(&p)
	c.Assert(err, check.IsNil)
	c.Assert(info.IP, check.Equals, "10.10.10.10")
	c.Assert(info.HTTPHostPort, check.Equals, "")
}

func (s *S) TestContainerShell(c *check.C) {
	container, err := s.newContainer(nil)
	c.Assert(err, check.IsNil)
	var stdout, stderr bytes.Buffer
	stdin := bytes.NewBufferString("")
	err = container.shell(s.p, stdin, &stdout, &stderr, pty{})
	c.Assert(err, check.IsNil)
	c.Assert(strings.Contains(stdout.String(), ""), check.Equals, true)
}

func (s *S) TestGetContainer(c *check.C) {
	coll := s.p.collection()
	defer coll.Close()
	coll.Insert(
		container{ID: "abcdef", Type: "python"},
		container{ID: "fedajs", Type: "ruby"},
		container{ID: "wat", Type: "java"},
	)
	defer coll.RemoveAll(bson.M{"id": bson.M{"$in": []string{"abcdef", "fedajs", "wat"}}})
	container, err := s.p.getContainer("abcdef")
	c.Assert(err, check.IsNil)
	c.Assert(container.ID, check.Equals, "abcdef")
	c.Assert(container.Type, check.Equals, "python")
	container, err = s.p.getContainer("wut")
	c.Assert(container, check.IsNil)
	c.Assert(err.Error(), check.Equals, "not found")
}

func (s *S) TestGetContainers(c *check.C) {
	coll := s.p.collection()
	defer coll.Close()
	coll.Insert(
		container{ID: "abcdef", Type: "python", AppName: "something"},
		container{ID: "fedajs", Type: "python", AppName: "something"},
		container{ID: "wat", Type: "java", AppName: "otherthing"},
	)
	defer coll.RemoveAll(bson.M{"id": bson.M{"$in": []string{"abcdef", "fedajs", "wat"}}})
	containers, err := s.p.listContainersByApp("something")
	c.Assert(err, check.IsNil)
	c.Assert(containers, check.HasLen, 2)
	c.Assert(containers[0].ID, check.Equals, "abcdef")
	c.Assert(containers[1].ID, check.Equals, "fedajs")
	containers, err = s.p.listContainersByApp("otherthing")
	c.Assert(err, check.IsNil)
	c.Assert(containers, check.HasLen, 1)
	c.Assert(containers[0].ID, check.Equals, "wat")
	containers, err = s.p.listContainersByApp("unknown")
	c.Assert(err, check.IsNil)
	c.Assert(containers, check.HasLen, 0)
}

func (s *S) TestGetImageFromAppPlatform(c *check.C) {
	app := provisiontest.NewFakeApp("myapp", "python", 1)
	img := s.p.getBuildImage(app)
	repoNamespace, err := config.GetString("docker:repository-namespace")
	c.Assert(err, check.IsNil)
	c.Assert(img, check.Equals, fmt.Sprintf("%s/python", repoNamespace))
}

func (s *S) TestGetImageAppWhenDeployIsMultipleOf10(c *check.C) {
	conn, err := db.Conn()
	c.Assert(err, check.IsNil)
	defer conn.Close()
	app := &app.App{Name: "app1", Platform: "python", Deploys: 20}
	err = conn.Apps().Insert(app)
	c.Assert(err, check.IsNil)
	defer conn.Apps().Remove(bson.M{"name": app.Name})
	cont := container{ID: "bleble", Type: app.Platform, AppName: app.Name, Image: "tsuru/app1"}
	coll := s.p.collection()
	err = coll.Insert(cont)
	c.Assert(err, check.IsNil)
	defer coll.Close()
	c.Assert(err, check.IsNil)
	defer coll.RemoveAll(bson.M{"id": cont.ID})
	img := s.p.getBuildImage(app)
	repoNamespace, err := config.GetString("docker:repository-namespace")
	c.Assert(err, check.IsNil)
	c.Assert(img, check.Equals, fmt.Sprintf("%s/%s", repoNamespace, app.Platform))
}

func (s *S) TestGetImageUseAppImageIfContainersExist(c *check.C) {
	cont := container{ID: "bleble", Type: "python", AppName: "myapp", Image: "ignored"}
	coll := s.p.collection()
	err := coll.Insert(cont)
	defer coll.Close()
	c.Assert(err, check.IsNil)
	defer coll.RemoveAll(bson.M{"id": "bleble"})
	app := provisiontest.NewFakeApp("myapp", "python", 1)
	img := s.p.getBuildImage(app)
	c.Assert(img, check.Equals, "tsuru/app-myapp")
}

func (s *S) TestGetImageWithRegistry(c *check.C) {
	config.Set("docker:registry", "localhost:3030")
	defer config.Unset("docker:registry")
	app := provisiontest.NewFakeApp("myapp", "python", 1)
	img := s.p.getBuildImage(app)
	repoNamespace, _ := config.GetString("docker:repository-namespace")
	expected := fmt.Sprintf("localhost:3030/%s/python", repoNamespace)
	c.Assert(img, check.Equals, expected)
}

func (s *S) TestContainerCommit(c *check.C) {
	cont, err := s.newContainer(nil)
	c.Assert(err, check.IsNil)
	defer s.removeTestContainer(cont)
	buf := bytes.Buffer{}
	nextImgName, err := appNewImageName(cont.AppName)
	c.Assert(err, check.IsNil)
	cont.BuildingImage = nextImgName
	imageId, err := cont.commit(s.p, &buf)
	c.Assert(err, check.IsNil)
	repoNamespace, _ := config.GetString("docker:repository-namespace")
	repository := repoNamespace + "/app-" + cont.AppName + ":v1"
	c.Assert(imageId, check.Equals, repository)
}

func (s *S) TestContainerCommitWithRegistry(c *check.C) {
	config.Set("docker:registry", "localhost:3030")
	defer config.Unset("docker:registry")
	cont, err := s.newContainer(nil)
	c.Assert(err, check.IsNil)
	defer s.removeTestContainer(cont)
	buf := bytes.Buffer{}
	nextImgName, err := appNewImageName(cont.AppName)
	c.Assert(err, check.IsNil)
	cont.BuildingImage = nextImgName
	imageId, err := cont.commit(s.p, &buf)
	c.Assert(err, check.IsNil)
	repoNamespace, _ := config.GetString("docker:repository-namespace")
	repository := "localhost:3030/" + repoNamespace + "/app-" + cont.AppName + ":v1"
	c.Assert(imageId, check.Equals, repository)
}

func (s *S) TestContainerCommitErrorInCommit(c *check.C) {
	s.server.PrepareFailure("commit-failure", "/commit")
	defer s.server.ResetFailure("commit-failure")
	cont, err := s.newContainer(nil)
	c.Assert(err, check.IsNil)
	defer s.removeTestContainer(cont)
	buf := bytes.Buffer{}
	nextImgName, err := appNewImageName(cont.AppName)
	c.Assert(err, check.IsNil)
	cont.BuildingImage = nextImgName
	_, err = cont.commit(s.p, &buf)
	c.Assert(err, check.ErrorMatches, ".*commit-failure\n")
}

func (s *S) TestContainerCommitErrorInPush(c *check.C) {
	s.server.PrepareFailure("push-failure", "/images/.*?/push")
	defer s.server.ResetFailure("push-failure")
	config.Set("docker:registry", "localhost:3030")
	defer config.Unset("docker:registry")
	cont, err := s.newContainer(nil)
	c.Assert(err, check.IsNil)
	defer s.removeTestContainer(cont)
	buf := bytes.Buffer{}
	nextImgName, err := appNewImageName(cont.AppName)
	c.Assert(err, check.IsNil)
	cont.BuildingImage = nextImgName
	_, err = cont.commit(s.p, &buf)
	c.Assert(err, check.ErrorMatches, ".*push-failure\n")
}

func (s *S) TestGitDeploy(c *check.C) {
	h := &apitest.TestHandler{}
	gandalfServer := repositorytest.StartGandalfTestServer(h)
	defer gandalfServer.Close()
	go s.stopContainers(1)
	err := s.newFakeImage(s.p, "tsuru/python")
	c.Assert(err, check.IsNil)
	app := provisiontest.NewFakeApp("myapp", "python", 1)
	routertest.FakeRouter.AddBackend(app.GetName())
	defer routertest.FakeRouter.RemoveBackend(app.GetName())
	var buf bytes.Buffer
	imageId, err := s.p.gitDeploy(app, "ff13e", &buf)
	c.Assert(err, check.IsNil)
	c.Assert(imageId, check.Equals, "tsuru/app-myapp:v1")
	var conts []container
	coll := s.p.collection()
	defer coll.Close()
	err = coll.Find(nil).All(&conts)
	c.Assert(err, check.IsNil)
	c.Assert(conts, check.HasLen, 0)
	err = s.p.getCluster().RemoveImage("tsuru/app-myapp:v1")
	c.Assert(err, check.IsNil)
}

type errBuffer struct{}

func (errBuffer) Write(data []byte) (int, error) {
	return 0, fmt.Errorf("My write error")
}

func (s *S) TestGitDeployRollsbackAfterErrorOnAttach(c *check.C) {
	h := &apitest.TestHandler{}
	gandalfServer := repositorytest.StartGandalfTestServer(h)
	defer gandalfServer.Close()
	go s.stopContainers(1)
	err := s.newFakeImage(s.p, "tsuru/python")
	c.Assert(err, check.IsNil)
	app := provisiontest.NewFakeApp("myapp", "python", 1)
	routertest.FakeRouter.AddBackend(app.GetName())
	defer routertest.FakeRouter.RemoveBackend(app.GetName())
	var buf errBuffer
	_, err = s.p.gitDeploy(app, "ff13e", &buf)
	c.Assert(err, check.ErrorMatches, ".*My write error")
	var conts []container
	coll := s.p.collection()
	defer coll.Close()
	err = coll.Find(nil).All(&conts)
	c.Assert(err, check.IsNil)
	c.Assert(conts, check.HasLen, 0)
	err = s.p.getCluster().RemoveImage("tsuru/myapp")
	c.Assert(err, check.NotNil)
}

func (s *S) TestArchiveDeploy(c *check.C) {
	go s.stopContainers(1)
	err := s.newFakeImage(s.p, "tsuru/python")
	c.Assert(err, check.IsNil)
	app := provisiontest.NewFakeApp("myapp", "python", 1)
	routertest.FakeRouter.AddBackend(app.GetName())
	defer routertest.FakeRouter.RemoveBackend(app.GetName())
	var buf bytes.Buffer
	_, err = s.p.archiveDeploy(app, s.p.getBuildImage(app), "https://s3.amazonaws.com/wat/archive.tar.gz", &buf)
	c.Assert(err, check.IsNil)
}

func (s *S) TestStart(c *check.C) {
	err := s.newFakeImage(s.p, "tsuru/python")
	c.Assert(err, check.IsNil)
	app := provisiontest.NewFakeApp("myapp", "python", 1)
	imageId := s.p.getBuildImage(app)
	routertest.FakeRouter.AddBackend(app.GetName())
	defer routertest.FakeRouter.RemoveBackend(app.GetName())
	var buf bytes.Buffer
	cont, err := s.p.start(app, imageId, &buf)
	c.Assert(err, check.IsNil)
	defer cont.remove(s.p)
	c.Assert(cont.ID, check.Not(check.Equals), "")
	cont2, err := s.p.getContainer(cont.ID)
	c.Assert(err, check.IsNil)
	c.Assert(cont2.Image, check.Equals, imageId)
	c.Assert(cont2.Status, check.Equals, provision.StatusStarting.String())
}

func (s *S) TestContainerStop(c *check.C) {
	cont, err := s.newContainer(nil)
	c.Assert(err, check.IsNil)
	defer s.removeTestContainer(cont)
	client, err := docker.NewClient(s.server.URL())
	c.Assert(err, check.IsNil)
	err = client.StartContainer(cont.ID, nil)
	c.Assert(err, check.IsNil)
	err = cont.stop(s.p)
	c.Assert(err, check.IsNil)
	dockerContainer, err := s.p.getCluster().InspectContainer(cont.ID)
	c.Assert(err, check.IsNil)
	c.Assert(dockerContainer.State.Running, check.Equals, false)
	c.Assert(cont.Status, check.Equals, provision.StatusStopped.String())
}

func (s *S) TestContainerStopReturnsNilWhenContainerAlreadyMarkedAsStopped(c *check.C) {
	cont, err := s.newContainer(nil)
	c.Assert(err, check.IsNil)
	defer s.removeTestContainer(cont)
	cont.setStatus(s.p, provision.StatusStopped.String())
	err = cont.stop(s.p)
	c.Assert(err, check.IsNil)
}

func (s *S) TestContainerLogs(c *check.C) {
	cont, err := s.newContainer(nil)
	c.Assert(err, check.IsNil)
	defer s.removeTestContainer(cont)
	var buff bytes.Buffer
	err = cont.logs(s.p, &buff)
	c.Assert(err, check.IsNil)
	c.Assert(buff.String(), check.Not(check.Equals), "")
}

func (s *S) TestUrlToHost(c *check.C) {
	var tests = []struct {
		input    string
		expected string
	}{
		{"http://localhost:8081", "localhost"},
		{"http://localhost:3234", "localhost"},
		{"http://10.10.10.10:2375", "10.10.10.10"},
		{"", ""},
	}
	for _, t := range tests {
		c.Check(urlToHost(t.input), check.Equals, t.expected)
	}
}

type NodeList []cluster.Node

func (a NodeList) Len() int           { return len(a) }
func (a NodeList) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a NodeList) Less(i, j int) bool { return a[i].Address < a[j].Address }

func (s *S) TestProvisionerGetCluster(c *check.C) {
	config.Set("docker:servers", []string{"http://localhost:2375", "http://10.10.10.10:2375"})
	defer config.Unset("docker:servers")
	config.Set("docker:cluster:redis-server", "127.0.0.1:6379")
	defer config.Unset("docker:cluster:redis-server")
	var p dockerProvisioner
	clus := p.getCluster()
	c.Assert(clus, check.NotNil)
	currentNodes, err := clus.Nodes()
	c.Assert(err, check.IsNil)
	sortedNodes := NodeList(currentNodes)
	sort.Sort(sortedNodes)
	c.Assert(sortedNodes, check.DeepEquals, NodeList([]cluster.Node{
		{Address: "http://10.10.10.10:2375", Metadata: map[string]string{}},
		{Address: "http://localhost:2375", Metadata: map[string]string{}},
	}))
}

func (s *S) TestProvisionerGetClusterSegregated(c *check.C) {
	config.Set("docker:segregate", true)
	defer config.Unset("docker:segregate")
	config.Set("docker:cluster:redis-server", "127.0.0.1:6379")
	defer config.Unset("docker:cluster:redis-server")
	var p dockerProvisioner
	clus := p.getCluster()
	c.Assert(clus, check.NotNil)
	currentNodes, err := clus.Nodes()
	c.Assert(err, check.IsNil)
	c.Assert(currentNodes, check.HasLen, 0)
}

func (s *S) TestGetDockerServersShouldSearchFromConfig(c *check.C) {
	config.Set("docker:servers", []string{"http://server01.com:2375", "http://server02.com:2375"})
	defer config.Unset("docker:servers")
	servers := getDockerServers()
	expected := []cluster.Node{
		{Address: "http://server01.com:2375"},
		{Address: "http://server02.com:2375"},
	}
	c.Assert(servers, check.DeepEquals, expected)
}

func (s *S) TestPushImage(c *check.C) {
	var requests []*http.Request
	server, err := testing.NewServer("127.0.0.1:0", nil, func(r *http.Request) {
		requests = append(requests, r)
	})
	c.Assert(err, check.IsNil)
	defer server.Stop()
	config.Set("docker:registry", "localhost:3030")
	defer config.Unset("docker:registry")
	var p dockerProvisioner
	p.cluster, err = cluster.New(nil, &cluster.MapStorage{},
		cluster.Node{Address: server.URL()})
	c.Assert(err, check.IsNil)
	err = s.newFakeImage(&p, "localhost:3030/base/img")
	c.Assert(err, check.IsNil)
	err = p.pushImage("localhost:3030/base/img", "")
	c.Assert(err, check.IsNil)
	c.Assert(requests, check.HasLen, 3)
	c.Assert(requests[0].URL.Path, check.Equals, "/images/create")
	c.Assert(requests[1].URL.Path, check.Equals, "/images/localhost:3030/base/img/json")
	c.Assert(requests[2].URL.Path, check.Equals, "/images/localhost:3030/base/img/push")
	c.Assert(requests[2].URL.RawQuery, check.Equals, "")
	err = s.newFakeImage(&p, "localhost:3030/base/img:v2")
	c.Assert(err, check.IsNil)
	err = p.pushImage("localhost:3030/base/img", "v2")
	c.Assert(err, check.IsNil)
	c.Assert(requests, check.HasLen, 6)
	c.Assert(requests[3].URL.Path, check.Equals, "/images/create")
	c.Assert(requests[4].URL.Path, check.Equals, "/images/localhost:3030/base/img:v2/json")
	c.Assert(requests[5].URL.Path, check.Equals, "/images/localhost:3030/base/img/push")
	c.Assert(requests[5].URL.RawQuery, check.Equals, "tag=v2")
}

func (s *S) TestPushImageNoRegistry(c *check.C) {
	var request *http.Request
	server, err := testing.NewServer("127.0.0.1:0", nil, func(r *http.Request) {
		request = r
	})
	c.Assert(err, check.IsNil)
	defer server.Stop()
	err = s.p.pushImage("localhost:3030/base", "")
	c.Assert(err, check.IsNil)
	c.Assert(request, check.IsNil)
}

func (s *S) TestContainerStart(c *check.C) {
	cont, err := s.newContainer(nil)
	c.Assert(err, check.IsNil)
	defer s.removeTestContainer(cont)
	client, err := docker.NewClient(s.server.URL())
	c.Assert(err, check.IsNil)
	contPath := fmt.Sprintf("/containers/%s/start", cont.ID)
	var restartPolicy string
	s.server.CustomHandler(contPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result := docker.HostConfig{}
		err := json.NewDecoder(r.Body).Decode(&result)
		if err == nil {
			restartPolicy = result.RestartPolicy.Name
		}
		s.server.DefaultHandler().ServeHTTP(w, r)
	}))
	defer s.server.CustomHandler(contPath, s.server.DefaultHandler())
	dockerContainer, err := client.InspectContainer(cont.ID)
	c.Assert(err, check.IsNil)
	c.Assert(dockerContainer.State.Running, check.Equals, false)
	err = cont.start(s.p, false)
	c.Assert(err, check.IsNil)
	dockerContainer, err = client.InspectContainer(cont.ID)
	c.Assert(err, check.IsNil)
	c.Assert(dockerContainer.State.Running, check.Equals, true)
	c.Assert(cont.Status, check.Equals, "starting")
	c.Assert(restartPolicy, check.Equals, "always")
}

func (s *S) TestContainerStartDeployContainer(c *check.C) {
	cont, err := s.newContainer(nil)
	c.Assert(err, check.IsNil)
	defer s.removeTestContainer(cont)
	contPath := fmt.Sprintf("/containers/%s/start", cont.ID)
	var restartPolicy string
	s.server.CustomHandler(contPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result := docker.HostConfig{}
		err := json.NewDecoder(r.Body).Decode(&result)
		if err == nil {
			restartPolicy = result.RestartPolicy.Name
		}
		s.server.DefaultHandler().ServeHTTP(w, r)
	}))
	defer s.server.CustomHandler(contPath, s.server.DefaultHandler())
	err = cont.start(s.p, true)
	c.Assert(err, check.IsNil)
	c.Assert(cont.Status, check.Equals, "building")
	c.Assert(restartPolicy, check.Equals, "")
}

func (s *S) TestContainerStartWithoutPort(c *check.C) {
	cont, err := s.newContainer(nil)
	c.Assert(err, check.IsNil)
	defer s.removeTestContainer(cont)
	oldUser, _ := config.Get("docker:run-cmd:port")
	defer config.Set("docker:run-cmd:port", oldUser)
	config.Unset("docker:run-cmd:port")
	err = cont.start(s.p, false)
	c.Assert(err, check.NotNil)
}

func (s *S) TestContainerStartStartedUnits(c *check.C) {
	cont, err := s.newContainer(nil)
	c.Assert(err, check.IsNil)
	defer s.removeTestContainer(cont)
	err = cont.start(s.p, false)
	c.Assert(err, check.IsNil)
	err = cont.start(s.p, false)
	c.Assert(err, check.NotNil)
}

func (s *S) TestContainerAvailable(c *check.C) {
	cases := map[provision.Status]bool{
		provision.StatusCreated:  false,
		provision.StatusStarting: true,
		provision.StatusStarted:  true,
		provision.StatusError:    false,
		provision.StatusStopped:  false,
		provision.StatusBuilding: false,
	}
	for status, expected := range cases {
		cont := container{Status: status.String()}
		c.Assert(cont.available(), check.Equals, expected)
	}
}

func (s *S) TestUnitFromContainer(c *check.C) {
	cont := container{
		ID:       "someid",
		AppName:  "someapp",
		Type:     "django",
		Status:   provision.StatusStarted.String(),
		HostAddr: "10.9.8.7",
	}
	expected := provision.Unit{
		Name:    cont.ID,
		AppName: cont.AppName,
		Type:    cont.Type,
		Status:  provision.Status(cont.Status),
		Ip:      cont.HostAddr,
	}
	c.Assert(unitFromContainer(cont), check.Equals, expected)
}

func (s *S) TestBuildClusterStorage(c *check.C) {
	defer config.Set("docker:cluster:mongo-url", "127.0.0.1:27017")
	defer config.Set("docker:cluster:mongo-database", "docker_provision_tests_cluster_stor")
	config.Unset("docker:cluster:mongo-url")
	_, err := buildClusterStorage()
	c.Assert(err, check.ErrorMatches, ".*docker:cluster:{mongo-url,mongo-database} must be set.")
	config.Set("docker:cluster:mongo-url", "127.0.0.1:27017")
	config.Unset("docker:cluster:mongo-database")
	_, err = buildClusterStorage()
	c.Assert(err, check.ErrorMatches, ".*docker:cluster:{mongo-url,mongo-database} must be set.")
	config.Set("docker:cluster:storage", "xxxx")
}

func (s *S) TestContainerExec(c *check.C) {
	container, err := s.newContainer(nil)
	c.Assert(err, check.IsNil)
	var stdout, stderr bytes.Buffer
	err = container.exec(s.p, &stdout, &stderr, "ls", "-lh")
	c.Assert(err, check.IsNil)
}

func (s *S) TestContainerExecErrorCode(c *check.C) {
	s.server.CustomHandler("/exec/id-exec-created-by-test/json", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ID": "id-exec-created-by-test", "ExitCode": 9}`))
	}))
	container, err := s.newContainer(nil)
	c.Assert(err, check.IsNil)
	var stdout, stderr bytes.Buffer
	err = container.exec(s.p, &stdout, &stderr, "ls", "-lh")
	c.Assert(err, check.DeepEquals, &execErr{code: 9})
}

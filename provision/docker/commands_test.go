// Copyright 2015 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package docker

import (
	"fmt"

	"github.com/tsuru/config"
	"github.com/tsuru/tsuru/api/apitest"
	"github.com/tsuru/tsuru/app/bind"
	"github.com/tsuru/tsuru/provision/provisiontest"
	"github.com/tsuru/tsuru/repository/repositorytest"
	"gopkg.in/check.v1"
)

func (s *S) TestGitDeployCmds(c *check.C) {
	h := &apitest.TestHandler{}
	h.Content = `{"git_url":"git://something/app-name.git"}`
	gandalfServer := repositorytest.StartGandalfTestServer(h)
	defer gandalfServer.Close()
	app := provisiontest.NewFakeApp("app-name", "python", 1)
	host_env := bind.EnvVar{
		Name:   "TSURU_HOST",
		Value:  "tsuru_host",
		Public: true,
	}
	token_env := bind.EnvVar{
		Name:   "TSURU_APP_TOKEN",
		Value:  "app_token",
		Public: true,
	}
	app.SetEnv(host_env)
	app.SetEnv(token_env)
	deployCmd, err := config.GetString("docker:deploy-cmd")
	c.Assert(err, check.IsNil)
	expectedPart1 := fmt.Sprintf("%s git git://something/app-name.git version", deployCmd)
	expectedAgent := fmt.Sprintf(`tsuru_unit_agent tsuru_host app_token app-name "%s" deploy`, expectedPart1)
	cmds, err := gitDeployCmds(app, "version")
	c.Assert(err, check.IsNil)
	c.Assert(cmds, check.DeepEquals, []string{"/bin/bash", "-lc", expectedAgent})
}

func (s *S) TestArchiveDeployCmds(c *check.C) {
	app := provisiontest.NewFakeApp("app-name", "python", 1)
	host_env := bind.EnvVar{
		Name:   "TSURU_HOST",
		Value:  "tsuru_host",
		Public: true,
	}
	token_env := bind.EnvVar{
		Name:   "TSURU_APP_TOKEN",
		Value:  "app_token",
		Public: true,
	}
	app.SetEnv(host_env)
	app.SetEnv(token_env)
	deployCmd, err := config.GetString("docker:deploy-cmd")
	c.Assert(err, check.IsNil)
	archiveURL := "https://s3.amazonaws.com/wat/archive.tar.gz"
	expectedPart1 := fmt.Sprintf("%s archive %s", deployCmd, archiveURL)
	expectedAgent := fmt.Sprintf(`tsuru_unit_agent tsuru_host app_token app-name "%s" deploy`, expectedPart1)
	cmds, err := archiveDeployCmds(app, archiveURL)
	c.Assert(err, check.IsNil)
	c.Assert(cmds, check.DeepEquals, []string{"/bin/bash", "-lc", expectedAgent})
}

func (s *S) TestRunWithAgentCmds(c *check.C) {
	app := provisiontest.NewFakeApp("app-name", "python", 1)
	host_env := bind.EnvVar{
		Name:   "TSURU_HOST",
		Value:  "tsuru_host",
		Public: true,
	}
	token_env := bind.EnvVar{
		Name:   "TSURU_APP_TOKEN",
		Value:  "app_token",
		Public: true,
	}
	app.SetEnv(host_env)
	app.SetEnv(token_env)
	runCmd, err := config.GetString("docker:run-cmd:bin")
	c.Assert(err, check.IsNil)
	unitAgentCmd := fmt.Sprintf("tsuru_unit_agent tsuru_host app_token app-name %s", runCmd)
	cmd := fmt.Sprintf("%s && tail -f /dev/null", unitAgentCmd)
	expected := []string{"/bin/bash", "-lc", cmd}
	cmds, err := runWithAgentCmds(app)
	c.Assert(err, check.IsNil)
	c.Assert(cmds, check.DeepEquals, expected)
}

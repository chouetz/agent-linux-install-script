// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package e2e

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/DataDog/datadog-agent/test/new-e2e/pkg/utils/e2e"
	"github.com/DataDog/test-infra-definitions/scenarios/aws/vm/ec2os"
	"github.com/DataDog/test-infra-definitions/scenarios/aws/vm/ec2params"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type osConfig struct {
	ami    string
	osType ec2os.Type
}

const (
	defaultScriptURL               = "https://s3.amazonaws.com/dd-agent/scripts"
	defaultAgentFlavor agentFlavor = agentFlavorDatadogAgent
	defaultPlatform                = "Ubuntu_22_04"
	defaultMode                    = "install"
)

var (
	// flags
	flavor    agentFlavor // datadog-agent, datadog-iot-agent, datadog-dogstatsd
	apiKey    string      // Needs to be valid, at least for the upgrade5 scenario
	scriptURL string      // To test a non-published script
	noFlush   bool        // To prevent eventual cleanup, to test install_script won't override existing configuration
	platform  string      // Platform under test

	baseNameByFlavor = map[agentFlavor]string{
		agentFlavorDatadogAgent:     "datadog-agent",
		agentFlavorDatadogDogstatsd: "datadog-dogstatsd",
		agentFlavorDatadogIOTAgent:  "datadog-agent",
	}
	configFileByFlavor = map[agentFlavor]string{
		agentFlavorDatadogAgent:     "datadog.yaml",
		agentFlavorDatadogDogstatsd: "dogstatsd.yaml",
		agentFlavorDatadogIOTAgent:  "datadog.yaml",
	}
	osConfigByPlatform = map[string]osConfig{
		"Debian_11":         {osType: ec2os.DebianOS},
		"Ubuntu_22_04":      {osType: ec2os.UbuntuOS},
		"RedHat_CentOS_7":   {osType: ec2os.CentOS},
		"RedHat_8":          {osType: ec2os.RedHatOS, ami: "ami-06640050dc3f556bb"},
		"Amazon_Linux_2023": {osType: ec2os.AmazonLinuxOS, ami: "ami-0889a44b331db0194"},
		"openSUSE_15":       {osType: ec2os.SuseOS},
	}
)

// note: no need to call flag.Parse() on test code, go test does it
func init() {
	flag.Var(&flavor, "flavor", "defines agent install flavor, supported values are [datadog-agent, datadog-iot-agent, datadog-dogstatsd]")
	flag.BoolVar(&noFlush, "noFlush", false, "To prevent eventual cleanup, to test install_script won't override existing configuration")
	flag.StringVar(&apiKey, "apiKey", os.Getenv("DD_API_KEY"), "Datadog API key")
	flag.StringVar(&scriptURL, "scriptURL", defaultScriptURL, fmt.Sprintf("Defines the script URL, default %s", defaultScriptURL))
	flag.StringVar(&platform, "platform", defaultPlatform, fmt.Sprintf("Defines the target platform, default %s", defaultPlatform))
}

type linuxInstallerTestSuite struct {
	e2e.Suite[e2e.VMEnv]
	baseName   string
	configFile string
	ec2Options []ec2params.Option
}

// SetupSuite is called at suite initialisation, once before all tests
func (s *linuxInstallerTestSuite) SetupSuite() {
	t := s.T()
	if _, ok := osConfigByPlatform[platform]; !ok {
		t.Skipf("not supported platform %s", platform)
	}
	if flavor == "" {
		t.Log("setting default agent flavor")
		flavor = defaultAgentFlavor
	}
	s.baseName = baseNameByFlavor[flavor]
	s.configFile = configFileByFlavor[flavor]
}

func (s *linuxInstallerTestSuite) assertInstallScript() {
	t := s.T()
	vm := s.Env().VM
	t.Log("Check user, config file and service")
	// check presence of the dd-agent user
	_, err := vm.ExecuteWithError("id dd-agent")
	assert.NoError(t, err, "user datadog-agent does not exist after install")
	// Check presence of the config file - the file is added by the install script, so this should always be okay
	// if the install succeeds
	_, err = vm.ExecuteWithError(fmt.Sprintf("stat /etc/%s/%s", s.baseName, s.configFile))
	assert.NoError(t, err, fmt.Sprintf("config file /etc/%s/%s does not exist after install", s.baseName, s.configFile))
	// Check presence and ownership of the config and main directories
	owner := strings.TrimSuffix(vm.Execute(fmt.Sprintf("stat -c \"%%U\" /etc/%s/", s.baseName)), "\n")
	assert.Equal(t, "dd-agent", owner, fmt.Sprintf("dd-agent does not own /etc/%s", s.baseName))
	owner = strings.TrimSuffix(vm.Execute(fmt.Sprintf("stat -c \"%%U\" /opt/%s/", s.baseName)), "\n")
	assert.Equal(t, "dd-agent", owner, fmt.Sprintf("dd-agent does not own /opt/%s", s.baseName))
	// Check that the service is active
	if _, err = vm.ExecuteWithError("command -v systemctl"); err == nil {
		_, err = vm.ExecuteWithError(fmt.Sprintf("systemctl is-active %s", s.baseName))
		assert.NoError(t, err, fmt.Sprintf("%s not running after Agent install", s.baseName))
	} else if _, err = vm.ExecuteWithError("command -v initctl"); err == nil {
		status := strings.TrimSuffix(vm.Execute(fmt.Sprintf("sudo status %s", s.baseName)), "\n")
		assert.Contains(t, "running", status, fmt.Sprintf("%s not running after Agent install", s.baseName))
	} else {
		require.FailNow(t, "Unknown service manager")
	}
}

func (s *linuxInstallerTestSuite) addExtraIntegration() {
	t := s.T()
	if flavor != "datadog-agent" {
		t.Skip()
	}
	vm := s.Env().VM
	t.Log("Install an extra integration, and create a custom file")
	_, err := vm.ExecuteWithError("sudo -u dd-agent -- datadog-agent integration install -t datadog-bind9==0.1.0")
	assert.NoError(t, err, "integration install failed")
	_ = vm.Execute(fmt.Sprintf("sudo -u dd-agent -- touch /opt/%s/embedded/lib/python3.9/site-packages/testfile", s.baseName))
}

func (s *linuxInstallerTestSuite) uninstall() {
	t := s.T()
	vm := s.Env().VM
	t.Logf("Remove %s", flavor)
	if _, err := vm.ExecuteWithError("command -v apt"); err == nil {
		t.Log("Uninstall with apt")
		vm.Execute(fmt.Sprintf("sudo apt remove -y %s", flavor))
	} else if _, err = vm.ExecuteWithError("command -v yum"); err == nil {
		t.Log("Uninstall with yum")
		vm.Execute(fmt.Sprintf("sudo yum remove -y %s", flavor))
	} else if _, err = vm.ExecuteWithError("command -v zypper"); err == nil {
		t.Log("Uninstall with zypper")
		vm.Execute(fmt.Sprintf("sudo zypper remove -y %s", flavor))
	} else {
		require.FailNow(t, "Unknown package manager")
	}
}

func (s *linuxInstallerTestSuite) assertUninstall() {
	t := s.T()
	vm := s.Env().VM
	t.Logf("Assert %s is removed", flavor)
	// dd-agent user and config file should still be here
	_, err := vm.ExecuteWithError("id dd-agent")
	assert.NoError(t, err, "user datadog-agent not present after remove")
	_, err = vm.ExecuteWithError(fmt.Sprintf("stat /etc/%s/%s", s.baseName, s.configFile))
	assert.NoError(t, err, fmt.Sprintf("/etc/%s/%s absent after remove", s.baseName, s.configFile))
	if flavor == "datadog-agent" {
		// The custom file should still be here. All other files, including the extra integration, should be removed
		_, err = vm.ExecuteWithError("stat /opt/datadog-agent/embedded/lib/python3.9/site-packages/testfile")
		assert.NoError(t, err, "testfile absent after remove")
		files := strings.Split(strings.TrimSuffix(vm.Execute("find /opt/datadog-agent -type f"), "\n"), "\n")
		assert.Len(t, files, 1, fmt.Sprintf("/opt/datadog-agent present after remove, found %v", files))
	} else {
		// All files in /opt/datadog-agent should be removed
		_, err = vm.ExecuteWithError(fmt.Sprintf("stat /opt/%s", s.baseName))
		assert.Error(t, err, fmt.Sprintf("/opt/%s present after remove", s.baseName))
	}
}

func (s *linuxInstallerTestSuite) purge() {
	t := s.T()
	vm := s.Env().VM

	if noFlush {
		t.Skip()
	}

	if _, err := vm.ExecuteWithError("command -v apt"); err != nil {
		t.Skip()
	}

	t.Log("Purge package")
	vm.Execute(fmt.Sprintf("sudo apt remove --purge -y %s", flavor))
}

func (s *linuxInstallerTestSuite) assertPurge() {
	t := s.T()
	vm := s.Env().VM

	if noFlush {
		t.Skip()
	}

	if _, err := vm.ExecuteWithError("command -v apt"); err != nil {
		t.Skip()
	}

	t.Log("Assert purge package")
	_, err := vm.ExecuteWithError("id datadog-agent")
	assert.Error(t, err, "dd-agent present after %s purge")
	_, err = vm.ExecuteWithError(fmt.Sprintf("stat /etc/%s", s.baseName))
	assert.Error(t, err, fmt.Sprintf("stat /etc/%s present after purge", s.baseName))
	_, err = vm.ExecuteWithError(fmt.Sprintf("stat /opt/%s", s.baseName))
	assert.Error(t, err, fmt.Sprintf("stat /opt/%s present after purge", s.baseName))
}
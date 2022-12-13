# Stepwise
**Vault testing Stepwise** was cloned from https://github.com/hashicorp/vault-testing-stepwise.

It provides a mini framework for writing acceptance tests for custom Vault plugins.

# Usage

The framework is simple, set the plugin name, which is used for building and mounting the plugin. Use that to create a new environment, and add test steps

```go
package pluginname

import (
	"os"
	"testing"

	stepwise "github.com/CSCfi/vault-testing-stepwise"
	"github.com/CSCfi/vault-testing-stepwise/environments/docker"
)

func TestPlugin(t *testing.T) {
	err := os.Setenv("VAULT_ACC", "1")
	if err != nil {
		t.Error("Failed to set environment variable VAULT_ACC")
	}
	
	// The mount options are also used for locating and building the plugin
	mountOptions := stepwise.MountOptions{
		MountPathPrefix: "plugin-mount-prefix",
		RegistryName:    "plugin-name-registry",
		PluginType:      stepwise.PluginTypeHere,
		PluginName:      "plugin-name",
	}
	keysCase := stepwise.Case{
		Environment:  docker.NewEnvironment("DockerPluginEnvironment", &mountOptions),
		SkipTeardown: false,
		Steps: []stepwise.Step{
			stepwise.Step{
				Name:      "testThatPathWrites",
				Operation: stepwise.WriteOperation,
				Data:      map[string]interface{}{"field": "value"},
				// Instead of passing static data, it's possible to use GetData with a function that returns the data    
				Path:      "/path/here",
			},
			stepwise.Step{
                Name:      "testThatPathReturnsX",
                Operation: stepwise.ReadOperation,
                Path:      "/path/here",
                Assert: func (resp *api.Secret, err error) error {
                
                    // resp.Data contains the `data` part of the response from Vault
                    
                    return nil
                },
            },
		},
	}
	
    // Running the case compiles the plugin with Docker, and runs Vault with the plugin enabled.
    // Each step in a case is run sequentially.
    // At the end of the case, the Docker container and network are removed, unless `SkipTeardown` is set to `true`
    stepwise.Run(t, keysCase)
}
```

# Debugging

Set `SkipTeardown` to `true`, and go into the container with `docker exec -it container-id sh`. Find the container ID with `docker ps`.

When leaving the container running, it's also possible to get the root token, and make API calls directly to Vault.
For that, run execute `stepwise.Run` instead, create an environment, run it's `Setup()` function, and print the `env.RootToken()` to the command line.

# Licensing

`vault-testing-stepwise` is licensed under MPL-2.0.

`SPDX-License-Identifier: MIT AND MPL-2.0`

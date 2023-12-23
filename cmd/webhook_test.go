package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"gopkg.in/yaml.v2"
	v1 "k8s.io/api/core/v1"
)

func TestLoadConfig(t *testing.T) {
	data := `
envs:
- configmapref:
    localobjectreference:
      name: env
namespaces:
- test
- default`
	var cfg Config
	err := yaml.Unmarshal([]byte(data), &cfg)
	if err != nil {
		t.Error(err)
	}
	expectedConfig := Config{
		EnvFromSources: []v1.EnvFromSource{{
			Prefix: "",
			ConfigMapRef: &v1.ConfigMapEnvSource{
				LocalObjectReference: v1.LocalObjectReference{Name: "env"},
				Optional:             nil,
			},
			SecretRef: nil,
		}},
		MutateNamespaces: []string{"test", "default"},
	}
	assert.Equal(t, expectedConfig, cfg)
}

package config

import (
	"mmx/internal/modules"
	"reflect"
	"testing"
)

func TestPercentTemplateKeepsSecretAndNodeReferencesVisible(t *testing.T) {
	task := Task{Module: "file", ModuleInputs: &modules.File{TemplateText: `cache={{ percent .node.memory.total_mib .secrets.cache_percent }}`}}
	if got := References(task); !reflect.DeepEqual(got, []string{".node.memory.total_mib", ".secrets.cache_percent"}) {
		t.Fatal(got)
	}
	if got := SecretNames(task); !reflect.DeepEqual(got, []string{"cache_percent"}) {
		t.Fatal(got)
	}
}

func TestChainedTemplateSecretIsNamed(t *testing.T) {
	task := Task{Module: "file", ModuleInputs: &modules.File{TemplateText: `password={{ (.secrets.creds).password }}`}}
	if got := SecretNames(task); !reflect.DeepEqual(got, []string{"creds"}) {
		t.Fatal(got)
	}
}

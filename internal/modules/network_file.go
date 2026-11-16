package modules

import (
	"fmt"
	"time"

	"mmx/internal/assets"
)

// NetworkFile is the whole-file input contract of the network modules. It
// shares input and template handling, not a network tool's activation policy.
// IfupdownInterfacesFile delegates template preparation here and adds its mode.
type NetworkFile struct {
	Name                                  string                    `yaml:"name" json:"name"`
	Content                               *string                   `yaml:"content,omitempty" json:"content,omitempty"`
	Template                              string                    `yaml:"template,omitempty" json:"template,omitempty"`
	State                                 string                    `yaml:"state,omitempty" json:"state,omitempty"`
	RestoreUnlessControllerConfirmsWithin string                    `yaml:"restore_unless_controller_confirms_within,omitempty" json:"restore_unless_controller_confirms_within,omitempty"`
	TemplateText                          string                    `yaml:"-" json:"template_text,omitempty"`
	Asset                                 *assets.Ref               `yaml:"-" json:"asset,omitempty"`
	LocalPath                             string                    `yaml:"-" json:"-"`
	SourceBase                            string                    `yaml:"-" json:"-"`
	Variants                              map[string]*SourceVariant `yaml:"-" json:"variants,omitempty"`
}

func (n NetworkFile) confirmationWait() (time.Duration, error) {
	if n.RestoreUnlessControllerConfirmsWithin == "" {
		return 2 * time.Minute, nil
	}
	wait, err := time.ParseDuration(n.RestoreUnlessControllerConfirmsWithin)
	if err != nil || wait <= 0 {
		return 0, fmt.Errorf("The duration for restoring an unconfirmed network file change must be positive.")
	}
	return wait, nil
}

// prepare checks the inputs; fileField is the input that names the file,
// name or path.
func (n *NetworkFile) prepare(base, fileField string) error {
	if err := validatePresence(n.State); err != nil {
		return err
	}
	if writtenDuration(n.RestoreUnlessControllerConfirmsWithin) {
		if _, err := n.confirmationWait(); err != nil {
			return err
		}
	}
	if n.State == "absent" {
		if n.Content != nil || n.Template != "" {
			return fmt.Errorf("Removing a network configuration file accepts only %s, state, and restore_unless_controller_confirms_within.", fileField)
		}
	} else if (n.Content == nil) == (n.Template == "") {
		return fmt.Errorf("a network configuration file requires exactly one of content or template.")
	}
	n.SourceBase = base
	if n.Template != "" && !Dynamic(n.Template) {
		path, err := localSource(base, "template", n.Template)
		if err != nil {
			return err
		}
		text, err := readTemplate(path)
		if err != nil {
			return &FieldError{"template", err}
		}
		n.LocalPath, n.TemplateText = path, string(text)
	}
	return nil
}

func (n *NetworkFile) Reads() Reads {
	if n.Template == "" {
		return Reads{}
	}
	return Reads{Templates: map[string]string{"template": n.TemplateText}}
}
func (n *NetworkFile) ControllerFiles() []ControllerFile {
	if n.Template == "" {
		return nil
	}
	return []ControllerFile{{Field: "template", Kind: SourceTemplate, Path: &n.Template, Base: n.SourceBase,
		Variants: &n.Variants, Asset: &n.Asset, Template: &n.TemplateText, Local: &n.LocalPath}}
}

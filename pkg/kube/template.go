package kube

import (
	"fmt"
	"strings"

	"github.com/IBM/argocd-vault-plugin/pkg/types"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8yaml "sigs.k8s.io/yaml"
)

// A Resource is the basis for all Templates
type Resource struct {
	Kind              string
	TemplateData      map[string]interface{} // The template as read from YAML
	replaceable       bool                   // Whether there are placeholders to replace or not; if false, VaultData will be nil
	replacementErrors []error                // Any errors encountered in performing replacements
	VaultData         map[string]interface{} // The data to replace with, from Vault
}

// Template is the template for Kubernetes
type Template struct {
	Resource
}

// NewTemplate returns a *Template given the template's data, and a VaultType
func NewTemplate(template map[string]interface{}, backend types.Backend, prefix string) (*Template, error) {
	obj := &unstructured.Unstructured{}
	err := kubeResourceDecoder(&template).Decode(&obj)
	if err != nil {
		return nil, fmt.Errorf("ToYAML: could not convert replaced template into %s: %s", obj.GetKind(), err)
	}

	path := fmt.Sprintf("%s/%s", prefix, strings.ToLower(obj.GetKind()))

	annotations := obj.GetAnnotations()
	if avpPath, ok := annotations["avp_path"]; ok {
		path = avpPath
	}

	var kvVersion string
	if kv, ok := annotations["kv_version"]; ok {
		kvVersion = kv
	}

	// Only worry about getting Vault secrets for templates with <placeholder>'s
	replaceable := replaceableInner(&template)
	var data map[string]interface{}
	if replaceable {
		data, err = backend.GetSecrets(path, kvVersion)
		if err != nil {
			return nil, err
		}
	}

	return &Template{
		Resource{
			Kind:         obj.GetKind(),
			TemplateData: template,
			replaceable:  replaceable,
			VaultData:    data,
		},
	}, nil
}

// Replace will replace the <placeholders> in the Template's data with values from Vault.
// It will return an aggregrate of any errors encountered during the replacements.
// For both non-Secret resources and Secrets with <placeholder>'s in `stringData`, the value in Vault is emitted as-is
// For Secret's with <placeholder>'s in `.data`, the value in Vault is emitted as base64
// For any hard-coded strings that aren't <placeholder>'s, the string is emitted as-is
func (t *Template) Replace() error {
	var replacerFunc func(string, string, map[string]interface{}) (interface{}, []error)

	if !t.replaceable {
		return nil
	}

	switch t.Kind {
	case "Secret":
		return t.secretReplace()
	case "ConfigMap":
		replacerFunc = configReplacement
	default:
		replacerFunc = genericReplacement
	}

	replaceInner(&t.Resource, &t.TemplateData, replacerFunc)
	if len(t.replacementErrors) != 0 {
		// TODO format multiple errors nicely
		return fmt.Errorf("Replace: could not replace all placeholders in Template: %s", t.replacementErrors)
	}
	return nil
}

func configReplacement(key, value string, vaultData map[string]interface{}) (interface{}, []error) {
	res, err := genericReplacement(key, value, vaultData)
	if err != nil {
		return nil, err
	}

	// configMap data values must be strings
	return stringify(res), err
}

// secretReplace will replace the <placeholders> in the template's data with values from Vault.
// It will return an aggregrate of any errors encountered during the replacements
// It will ensure that `<placeholder>`'s in `.data` are base64 encoded
func (t *Template) secretReplace() error {

	// Replace metadata normally
	metadata, ok := t.TemplateData["metadata"].(map[string]interface{})
	if ok {
		replaceInner(&t.Resource, &metadata, genericReplacement)
		if len(t.replacementErrors) != 0 {

			// TODO format multiple errors nicely
			return fmt.Errorf("Replace: could not replace all placeholders in SecretTemplate metadata: %s", t.replacementErrors)
		}
	}

	// Replace stringData normally
	stringData, ok := t.TemplateData["stringData"].(map[string]interface{})
	if ok {
		replaceInner(&t.Resource, &stringData, genericReplacement)
		if len(t.replacementErrors) != 0 {

			// TODO format multiple errors nicely
			return fmt.Errorf("Replace: could not replace all placeholders in SecretTemplate stringData: %s", t.replacementErrors)
		}
	}

	// Replace <placeholder>'d Secret.data with []byte's
	data, ok := t.TemplateData["data"].(map[string]interface{})
	if ok {
		replaceInner(&t.Resource, &data, func(key, value string, vaultData map[string]interface{}) (_ interface{}, err []error) {
			res, err := genericReplacement(key, value, vaultData)

			// We have to return []byte for k8s secrets,
			// so we convert everything that came from Vault
			// Strings hardcoded in the Secret.data are assumed to be base64 encoded already
			if placeholder.Match([]byte(value)) {
				return []byte(stringify(res)), err
			}
			return res, err
		})

		if len(t.replacementErrors) != 0 {

			// TODO format multiple errors nicely
			return fmt.Errorf("Replace: could not replace all placeholders in SecretTemplate data: %s", t.replacementErrors)
		}
	}

	return nil
}

// ToYAML seralizes the completed template into YAML to be consumed by Kubernetes
func (t *Template) ToYAML() (string, error) {
	obj := &unstructured.Unstructured{}
	err := kubeResourceDecoder(&t.TemplateData).Decode(&obj)
	if err != nil {
		return "", fmt.Errorf("ToYAML: could not convert replaced template into %s: %s", obj.GetKind(), err)
	}
	res, err := k8yaml.Marshal(&obj)
	if err != nil {
		return "", fmt.Errorf("ToYAML: could not export %s into YAML: %s", obj.GetKind(), err)
	}
	return string(res), nil
}

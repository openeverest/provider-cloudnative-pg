package provider

import (
	"fmt"
	"strconv"
	"strings"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-cloudnative-pg/definition/components"
)

var userSecretRefPath = field.NewPath("spec", "userSecretRef")

// userSecretKeys are the keys CloudNativePG reads the owner's credentials from.
var userSecretKeys = []string{"username", "password"}

// applyUserSecret names the Secret spec.userSecretRef refers to as the
// application database owner's Secret, the one CloudNativePG sets the owner's
// password from and the connection details are read from.
func applyUserSecret(c *controller.Context, custom *components.CNPGCustomSpec) error {
	name, _, err := userSecretName(c, custom)
	if err != nil || name == "" {
		return err
	}
	if custom.Bootstrap == nil {
		custom.Bootstrap = &components.BootstrapConfiguration{}
	}
	if custom.Bootstrap.InitDB == nil {
		custom.Bootstrap.InitDB = &cnpgv1.BootstrapInitDB{}
	}
	custom.Bootstrap.InitDB.Secret = &cnpgv1.LocalObjectReference{Name: name}
	return nil
}

// userSecretName is the Secret spec.userSecretRef refers to when it seeds the
// owner's credentials, else "", and whether the Cluster exists. It seeds them
// only when the engine parameters name no initdb Secret of their own, and only
// for a new Cluster or one already created with it: an existing Cluster keeps
// the credentials it has.
func userSecretName(c *controller.Context, custom *components.CNPGCustomSpec) (name string, exists bool, err error) {
	ref := c.Instance().Spec.UserSecretRef
	if ref == nil || initDBSecretName(custom) != "" {
		return "", false, nil
	}
	live := &cnpgv1.Cluster{}
	if err := c.Get(live, c.Name()); err != nil {
		if !apierrors.IsNotFound(err) {
			return "", false, fmt.Errorf("get CloudNativePG Cluster %q: %w", c.Name(), err)
		}
		return ref.Name, false, nil
	}
	if live.GetApplicationSecretName() != ref.Name {
		return "", true, nil
	}
	return ref.Name, true, nil
}

// validateUserSecret checks, before the Cluster is created, that the Secret
// spec.userSecretRef refers to holds a username and a password, and that the
// username is the application database owner, as CloudNativePG requires.
// The runtime reports a missing Secret itself.
func validateUserSecret(c *controller.Context, custom *components.CNPGCustomSpec) (field.ErrorList, error) {
	name, exists, err := userSecretName(c, custom)
	if err != nil || name == "" || exists {
		return nil, err
	}

	secret, err := optionalSecret(c, name)
	if err != nil || secret == nil {
		return nil, err
	}
	namePath := userSecretRefPath.Child("name")
	var missing []string
	for _, key := range userSecretKeys {
		if secretValue(secret, key) == "" {
			missing = append(missing, strconv.Quote(key))
		}
	}
	if len(missing) > 0 {
		return field.ErrorList{field.Invalid(namePath, name, "missing "+strings.Join(missing, ", "))}, nil
	}
	if username, owner := secretValue(secret, "username"), applicationOwner(custom); username != owner {
		return field.ErrorList{field.Invalid(namePath, name,
			fmt.Sprintf("username %q must be the application database owner %q", username, owner))}, nil
	}
	return nil, nil
}

// userSecretWarnings explains when spec.userSecretRef is ignored: the engine
// parameters name the owner's Secret themselves, or it is added to an
// existing Instance, whose credentials are already set.
func userSecretWarnings(instance, old *corev1alpha1.Instance, custom *components.CNPGCustomSpec) []string {
	if instance.Spec.UserSecretRef == nil {
		return nil
	}
	if initDBSecretName(custom) != "" {
		return []string{userSecretRefPath.String() + ": ignored, " +
			engineParams.Child("bootstrap", "initdb", "secret").String() + " names the application owner's Secret"}
	}
	if old != nil && old.Spec.UserSecretRef == nil {
		return []string{userSecretRefPath.String() + ": ignored, it only applies when the instance is created"}
	}
	return nil
}

func initDBSecretName(custom *components.CNPGCustomSpec) string {
	if custom.Bootstrap == nil || custom.Bootstrap.InitDB == nil || custom.Bootstrap.InitDB.Secret == nil {
		return ""
	}
	return custom.Bootstrap.InitDB.Secret.Name
}

// applicationOwner is the owner of the application database, defaulted the
// way CloudNativePG does: the database name, else app.
func applicationOwner(custom *components.CNPGCustomSpec) string {
	if custom.Bootstrap != nil && custom.Bootstrap.InitDB != nil {
		initDB := custom.Bootstrap.InitDB
		if initDB.Owner != "" {
			return initDB.Owner
		}
		if initDB.Database != "" {
			return initDB.Database
		}
	}
	return cnpgv1.DefaultApplicationUserName
}

// secretValue is the value of key, stringData first as Kubernetes merges it.
func secretValue(secret *corev1.Secret, key string) string {
	if v, ok := secret.StringData[key]; ok {
		return v
	}
	return string(secret.Data[key])
}

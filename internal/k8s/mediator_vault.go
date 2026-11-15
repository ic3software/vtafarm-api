package k8s

import (
	"reflect"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

const (
	VaultCAConfigMapName = "vtafarm-vault-ca"
	VaultCAConfigMapKey  = "ca.crt"
	VaultCAVolumeName    = "vault-ca"
	VaultCAMountPath     = "/etc/vtafarm/vault"
	VaultCAFilePath      = VaultCAMountPath + "/" + VaultCAConfigMapKey
	VaultCAEnvName       = "VAULT_CACERT"

	mediatorVaultMigrationInit = "migrate-vault-ca-config"
)

const mediatorVaultMigrationScript = `set -eu
config=/work/mediator/conf/mediator.toml
test -f "$config"
if grep -Fq '&insecure=1' "$config"; then
  sed -i 's|&insecure=1||g' "$config"
fi
if grep -Fq '&ca_cert=' "$config"; then
  sed -i 's|&ca_cert=[^&"]*||g' "$config"
fi
! grep -Eq '&(insecure=1|ca_cert=)' "$config"
`

// VaultCAVolume and VaultCAVolumeMount expose trust-manager's public CA
// ConfigMap without granting a workload access to the Vault namespace or its
// CA private key.
func VaultCAVolume() corev1.Volume {
	return corev1.Volume{
		Name: VaultCAVolumeName,
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: VaultCAConfigMapName},
			Items:                []corev1.KeyToPath{{Key: VaultCAConfigMapKey, Path: VaultCAConfigMapKey}},
		}},
	}
}

func VaultCAVolumeMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: VaultCAVolumeName, MountPath: VaultCAMountPath, ReadOnly: true}
}

func VaultCAEnvVar() corev1.EnvVar {
	return corev1.EnvVar{Name: VaultCAEnvName, Value: VaultCAFilePath}
}

// ensureMediatorVaultCA brings an existing full-stack mediator onto the same
// configuration used for new sessions. VAULT_CACERT is understood by both the
// pre-hardening 0.52 image and 0.54, so the persisted URL needs no
// version-specific query parameter and remains safe to roll back.
func ensureMediatorVaultCA(deployment *appsv1.Deployment, image string) bool {
	if deployment.Labels["app"] != "fs-mediator" || len(deployment.Spec.Template.Spec.Containers) == 0 {
		return false
	}

	pod := &deployment.Spec.Template.Spec
	container := &pod.Containers[0]
	changed := upsertVolume(&pod.Volumes, VaultCAVolume())
	if upsertVolumeMount(&container.VolumeMounts, VaultCAVolumeMount()) {
		changed = true
	}

	desiredEnv := VaultCAEnvVar()
	filteredEnv := container.Env[:0]
	foundDesiredEnv := false
	for _, env := range container.Env {
		switch env.Name {
		case "MEDIATOR_ALLOW_INSECURE_VAULT":
			changed = true
			continue
		case VaultCAEnvName:
			if !foundDesiredEnv && reflect.DeepEqual(env, desiredEnv) {
				filteredEnv = append(filteredEnv, env)
				foundDesiredEnv = true
				continue
			}
			changed = true
			continue
		}
		filteredEnv = append(filteredEnv, env)
	}
	if !foundDesiredEnv {
		filteredEnv = append(filteredEnv, desiredEnv)
		changed = true
	}
	container.Env = filteredEnv

	var dataMount *corev1.VolumeMount
	for i := range container.VolumeMounts {
		if container.VolumeMounts[i].MountPath == "/work/mediator" {
			mount := container.VolumeMounts[i]
			dataMount = &mount
			break
		}
	}
	if dataMount == nil {
		return changed
	}

	desired := corev1.Container{
		Name:         mediatorVaultMigrationInit,
		Image:        image,
		Command:      []string{"sh", "-c", mediatorVaultMigrationScript},
		WorkingDir:   "/work/mediator",
		VolumeMounts: []corev1.VolumeMount{*dataMount},
	}
	for i := range pod.InitContainers {
		if pod.InitContainers[i].Name != mediatorVaultMigrationInit {
			continue
		}
		if !reflect.DeepEqual(pod.InitContainers[i], desired) {
			pod.InitContainers[i] = desired
			changed = true
		}
		return changed
	}
	pod.InitContainers = append(pod.InitContainers, desired)
	return true
}

func upsertVolume(volumes *[]corev1.Volume, desired corev1.Volume) bool {
	for i := range *volumes {
		if (*volumes)[i].Name != desired.Name {
			continue
		}
		if reflect.DeepEqual((*volumes)[i], desired) {
			return false
		}
		(*volumes)[i] = desired
		return true
	}
	*volumes = append(*volumes, desired)
	return true
}

func upsertVolumeMount(mounts *[]corev1.VolumeMount, desired corev1.VolumeMount) bool {
	for i := range *mounts {
		if (*mounts)[i].Name != desired.Name {
			continue
		}
		if reflect.DeepEqual((*mounts)[i], desired) {
			return false
		}
		(*mounts)[i] = desired
		return true
	}
	*mounts = append(*mounts, desired)
	return true
}

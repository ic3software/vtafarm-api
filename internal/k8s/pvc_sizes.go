package k8s

// PVC storage requests for newly provisioned session components. Existing
// claims are left unchanged because all creation paths are idempotent.
const (
	VtaPVCStorageSize        = "200Mi"
	DIDHostingPVCStorageSize = "200Mi"
	MediatorPVCStorageSize   = "1Gi"
	VtcPVCStorageSize        = "200Mi"
)

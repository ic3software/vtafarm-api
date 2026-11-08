package model

import (
	"encoding/json"
	"time"
)

const (
	PlacementPlanReconcile = "reconcile"
	PlacementPlanRelocate  = "relocate"
	PlacementPlanProvision = "provision"

	PlacementPlanRunning   = "running"
	PlacementPlanPaused    = "paused"
	PlacementPlanCompleted = "completed"
	PlacementPlanCancelled = "cancelled"

	PlacementRequested             = "requested"
	PlacementValidating            = "validating"
	PlacementReserved              = "reserved"
	PlacementScalingDown           = "scaling_down"
	PlacementWaitingPodTermination = "waiting_for_pod_termination"
	PlacementWaitingVolumeDetach   = "waiting_for_volume_detach"
	PlacementCreatingGatedPod      = "creating_gated_pod"
	PlacementAssigningTargetNode   = "assigning_target_node"
	PlacementWaitingVolumeAttach   = "waiting_for_volume_attach"
	PlacementWaitingReady          = "waiting_for_ready"
	PlacementCompleted             = "completed"
	PlacementFailed                = "failed"
	PlacementRollingBack           = "rolling_back"
	PlacementRolledBack            = "rolled_back"
	PlacementCancelled             = "cancelled"

	PlacementEventTaskState  = "task_state_changed"
	PlacementEventPlanStatus = "plan_status_changed"

	PlacementActorAdmin  = "admin"
	PlacementActorWorker = "worker"
	PlacementActorSystem = "system"
)

type PlacementPlan struct {
	ID        uint             `gorm:"primaryKey;autoIncrement" json:"id"`
	AdminID   *uint            `json:"-"`
	Kind      string           `gorm:"not null"                 json:"kind"`
	Status    string           `gorm:"not null"                 json:"status"`
	ErrorMsg  string           `gorm:"not null;default:''"      json:"error_msg,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
	UpdatedAt time.Time        `json:"updated_at"`
	Tasks     []PlacementTask  `gorm:"foreignKey:PlanID" json:"tasks,omitempty"`
	Events    []PlacementEvent `gorm:"foreignKey:PlanID" json:"events,omitempty"`
}

type PlacementTask struct {
	ID                 uint       `gorm:"primaryKey;autoIncrement" json:"id"`
	PlanID             uint       `gorm:"not null;index"           json:"plan_id"`
	SessionID          uint       `gorm:"not null;index"           json:"session_id"`
	Component          string     `gorm:"not null;default:vta"     json:"component"`
	SourceNode         string     `gorm:"not null;default:''"      json:"source_node"`
	TargetNode         string     `gorm:"not null;default:''"      json:"target_node"`
	State              string     `gorm:"not null;default:requested" json:"state"`
	ErrorMsg           string     `gorm:"not null;default:''"      json:"error_msg,omitempty"`
	P95MemoryBytes     int64      `gorm:"not null;default:0"       json:"p95_memory_bytes"`
	OriginalReplicas   int32      `gorm:"not null;default:1"       json:"original_replicas"`
	DeploymentSnapshot []byte     `gorm:"type:jsonb;not null;default:'{}'" json:"-"`
	Namespace          string     `gorm:"not null;default:''"      json:"namespace"`
	DeploymentName     string     `gorm:"not null;default:''"      json:"deployment_name"`
	DeploymentUID      string     `gorm:"not null;default:''"      json:"deployment_uid"`
	PVCName            string     `gorm:"not null;default:''"      json:"pvc_name"`
	PVCUID             string     `gorm:"not null;default:''"      json:"pvc_uid"`
	PVName             string     `gorm:"not null;default:''"      json:"pv_name"`
	PVUID              string     `gorm:"not null;default:''"      json:"pv_uid"`
	CSIVolumeHandle    string     `gorm:"not null;default:''"      json:"csi_volume_handle"`
	LonghornVolume     string     `gorm:"not null;default:''"      json:"longhorn_volume"`
	LastBackupName     string     `gorm:"not null;default:''"      json:"last_backup_name,omitempty"`
	LastBackupAt       string     `gorm:"not null;default:''"      json:"last_backup_at,omitempty"`
	PodUID             string     `gorm:"not null;default:''"      json:"pod_uid"`
	StartedAt          *time.Time `json:"started_at,omitempty"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

type PlacementReservation struct {
	ID          uint       `gorm:"primaryKey;autoIncrement" json:"id"`
	TaskID      uint       `gorm:"not null;uniqueIndex"     json:"task_id"`
	NodeName    string     `gorm:"not null;index"           json:"node_name"`
	MemoryBytes int64      `gorm:"not null"                 json:"memory_bytes"`
	PodUID      string     `gorm:"not null;default:''"      json:"pod_uid,omitempty"`
	Status      string     `gorm:"not null;default:active"  json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	ReleasedAt  *time.Time `json:"released_at,omitempty"`
}

type PlacementEvent struct {
	ID        uint            `gorm:"primaryKey;autoIncrement" json:"id"`
	PlanID    uint            `gorm:"not null;index" json:"plan_id"`
	TaskID    *uint           `json:"task_id,omitempty"`
	EventType string          `gorm:"not null" json:"event_type"`
	FromState string          `gorm:"not null;default:''" json:"from_state,omitempty"`
	State     string          `gorm:"not null" json:"state"`
	ActorType string          `gorm:"not null" json:"actor_type"`
	ActorID   *uint           `json:"actor_id,omitempty"`
	Message   string          `gorm:"not null;default:''" json:"message,omitempty"`
	Metadata  json.RawMessage `gorm:"type:jsonb;not null;default:'{}'" json:"metadata"`
	CreatedAt time.Time       `json:"created_at"`
}

type PlacementMaintenanceLock struct {
	SessionID  uint      `gorm:"primaryKey" json:"session_id"`
	TaskID     uint      `gorm:"not null;uniqueIndex" json:"task_id"`
	Operation  string    `gorm:"not null" json:"operation"`
	AcquiredAt time.Time `json:"acquired_at"`
}

type PlacementNodeState struct {
	NodeName      string     `gorm:"primaryKey" json:"node_name"`
	MemoryState   string     `gorm:"not null;default:unknown" json:"memory_state"`
	Eligible      bool       `gorm:"not null;default:false" json:"eligible"`
	HotSince      *time.Time `json:"hot_since,omitempty"`
	RecoverySince *time.Time `json:"recovery_since,omitempty"`
	ObservedAt    *time.Time `json:"observed_at,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

package handler

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/ic3software/vtafarm-api/internal/model"
)

const (
	aclListBeginMarker = "VTAFARM_ACL_LIST_BEGIN"
	aclListEndMarker   = "VTAFARM_ACL_LIST_END"
)

type sessionAclResponse struct {
	Entries  []model.VtaAclEntry `json:"entries"`
	SyncedAt *time.Time          `json:"synced_at"`
	Warning  string              `json:"warning,omitempty"`
}

const superAdminAclRole = "admin (super admin)"

// ListSessionAdmins returns the super admins from the last complete VTA ACL
// snapshot without causing downtime. An absent snapshot is represented by a
// nil timestamp and empty entries, so the portal can offer the first refresh.
func (h *SetupHandler) ListSessionAdmins(c *gin.Context) {
	session := h.userSession(c)
	if session == nil {
		return
	}

	response, err := h.sessionAclSnapshot(session.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read the VTA ACL snapshot"})
		return
	}
	c.JSON(http.StatusOK, response)
}

// RefreshSessionAdmins stops the VTA, reads its complete local ACL, atomically
// replaces the database snapshot, and restarts the VTA.
func (h *SetupHandler) RefreshSessionAdmins(c *gin.Context) {
	session := h.userSession(c)
	if session == nil {
		return
	}
	if session.Status != "running" {
		c.JSON(http.StatusConflict, gin.H{"error": "session must be in running status"})
		return
	}

	h.refreshVtaAclSnapshot(c, session)
}

func (h *SetupHandler) refreshVtaAclSnapshot(c *gin.Context, session *model.SetupSession) {
	logs, restartErr, runErr := h.runVtaAclJob(c.Request.Context(), session, aclListCmd())
	if runErr != nil {
		respondAclJobError(c, session, runErr, restartErr)
		return
	}

	entries, parseErr := parseVtaAclList(logs)
	if parseErr != nil {
		respondAclJobError(c, session, fmt.Errorf("failed to parse vta acl list output: %w", parseErr), restartErr)
		return
	}
	if err := h.syncSessionAclSnapshot(session.ID, entries); err != nil {
		message := "failed to save the VTA ACL snapshot"
		if restartErr != nil {
			message += " — " + restartWarning(session, restartErr)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": message})
		return
	}

	response, err := h.sessionAclSnapshot(session.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read the VTA ACL snapshot"})
		return
	}
	if restartErr != nil {
		response.Warning = restartWarning(session, restartErr)
	}
	c.JSON(http.StatusOK, response)
}

func aclListCmd() string {
	return "set -e\n" +
		"echo " + aclListBeginMarker + "\n" +
		"vta acl list 2>&1\n" +
		"echo " + aclListEndMarker + "\n"
}

func parseVtaAclList(logs string) ([]model.VtaAclEntry, error) {
	lines := strings.Split(strings.ReplaceAll(logs, "\r\n", "\n"), "\n")
	start := -1
	end := -1
	for i, line := range lines {
		switch strings.TrimSpace(line) {
		case aclListBeginMarker:
			start = i
		case aclListEndMarker:
			if start >= 0 {
				end = i
			}
		}
	}
	if start < 0 {
		return nil, errors.New("start marker not found")
	}
	if end < 0 {
		return nil, errors.New("end marker not found")
	}
	if end <= start {
		return nil, errors.New("ACL list markers are out of order")
	}

	entries := make([]model.VtaAclEntry, 0)
	var current *model.VtaAclEntry
	seen := make(map[string]struct{})
	declaredCount := -1
	emptyList := false
	flush := func() error {
		if current == nil {
			return nil
		}
		if current.Did == "" || current.Role == "" || current.Contexts == "" || current.AclCreatedAt == "" {
			return fmt.Errorf("incomplete ACL entry for %q", current.Did)
		}
		if _, exists := seen[current.Did]; exists {
			return fmt.Errorf("duplicate ACL entry for %q", current.Did)
		}
		seen[current.Did] = struct{}{}
		entries = append(entries, *current)
		current = nil
		return nil
	}

	for _, line := range lines[start+1 : end] {
		line = strings.TrimSpace(line)
		switch {
		case line == "No ACL entries found.":
			emptyList = true
		case strings.HasSuffix(line, " ACL entries:"):
			count, err := strconv.Atoi(strings.TrimSuffix(line, " ACL entries:"))
			if err != nil || count < 0 {
				return nil, fmt.Errorf("invalid ACL entry count %q", line)
			}
			declaredCount = count
		case strings.HasPrefix(line, "DID:"):
			if err := flush(); err != nil {
				return nil, err
			}
			current = &model.VtaAclEntry{Did: strings.TrimSpace(strings.TrimPrefix(line, "DID:"))}
		case current != nil && strings.HasPrefix(line, "Role:"):
			current.Role = strings.TrimSpace(strings.TrimPrefix(line, "Role:"))
		case current != nil && strings.HasPrefix(line, "Label:"):
			current.Label = strings.TrimSpace(strings.TrimPrefix(line, "Label:"))
		case current != nil && strings.HasPrefix(line, "Contexts:"):
			current.Contexts = strings.TrimSpace(strings.TrimPrefix(line, "Contexts:"))
		case current != nil && strings.HasPrefix(line, "Created:"):
			current.AclCreatedAt = strings.TrimSpace(strings.TrimPrefix(line, "Created:"))
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if !emptyList && declaredCount < 0 {
		return nil, errors.New("ACL entry count not found")
	}
	if emptyList && len(entries) != 0 {
		return nil, errors.New("empty ACL marker was followed by entries")
	}
	if declaredCount >= 0 && declaredCount != len(entries) {
		return nil, fmt.Errorf("ACL entry count was %d, parsed %d", declaredCount, len(entries))
	}
	return entries, nil
}

func (h *SetupHandler) syncSessionAclSnapshot(sessionID uint, entries []model.VtaAclEntry) error {
	now := time.Now()
	return h.db.Transaction(func(tx *gorm.DB) error {
		snapshot := model.VtaAclSnapshot{SessionID: sessionID, SyncedAt: &now, EntryCount: len(entries)}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "session_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"synced_at", "entry_count"}),
		}).Create(&snapshot).Error; err != nil {
			return err
		}
		if err := tx.Where("session_id = ?", sessionID).Delete(&model.VtaAclEntry{}).Error; err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		for i := range entries {
			entries[i].SessionID = sessionID
		}
		return tx.Create(&entries).Error
	})
}

func (h *SetupHandler) sessionAclSnapshot(sessionID uint) (sessionAclResponse, error) {
	response := sessionAclResponse{Entries: []model.VtaAclEntry{}}
	var snapshot model.VtaAclSnapshot
	err := h.db.Where("session_id = ?", sessionID).First(&snapshot).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return response, nil
	}
	if err != nil {
		return response, err
	}
	var entries []model.VtaAclEntry
	response.SyncedAt = snapshot.SyncedAt
	if err := h.db.Where("session_id = ?", sessionID).Order("did ASC").Find(&entries).Error; err != nil {
		return response, err
	}
	if len(entries) != snapshot.EntryCount {
		return response, fmt.Errorf("ACL snapshot expected %d entries, found %d", snapshot.EntryCount, len(entries))
	}
	response.Entries = superAdminAclEntries(entries)
	sortVtaAclEntriesNewestFirst(response.Entries)
	return response, nil
}

func superAdminAclEntries(entries []model.VtaAclEntry) []model.VtaAclEntry {
	filtered := make([]model.VtaAclEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Role == superAdminAclRole {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func sortVtaAclEntriesNewestFirst(entries []model.VtaAclEntry) {
	const layout = "2006-01-02 15:04:05 -07:00"
	sort.SliceStable(entries, func(i, j int) bool {
		left, leftErr := time.Parse(layout, entries[i].AclCreatedAt)
		right, rightErr := time.Parse(layout, entries[j].AclCreatedAt)
		if leftErr == nil && rightErr == nil {
			return left.After(right)
		}
		if leftErr == nil {
			return true
		}
		if rightErr == nil {
			return false
		}
		return entries[i].Did < entries[j].Did
	})
}

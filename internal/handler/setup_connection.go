package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/siop/webvh"
)

type connectionTarget struct {
	infra        sharedInfra
	source       string
	providerID   *uint
	providerName string
}

type connectionDIDs struct {
	DIDHostingDid string `json:"did_hosting_did" binding:"required"`
	MediatorDid   string `json:"mediator_did" binding:"required"`
}

// InspectConnection classifies DID hosting by its daemon DID. The mediator DID
// may be supplied independently, including one hosted elsewhere.
func (h *SetupHandler) InspectConnection(c *gin.Context) {
	var req connectionDIDs
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	target, status, err := h.resolveCustomConnection(c.Request.Context(), req.DIDHostingDid, req.MediatorDid)
	if err != nil {
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"source":             target.source,
		"provider":           target.providerName,
		"did_hosting_url":    target.infra.ServerURL,
		"did_hosting_did":    target.infra.DaemonDid,
		"mediator_did":       target.infra.MediatorDid,
		"manual_publication": target.source == model.ConnectionExternal,
	})
}

func (h *SetupHandler) resolveCustomConnection(ctx context.Context, hostingDID, mediatorDID string) (connectionTarget, int, error) {
	hostingDID = strings.TrimSpace(hostingDID)
	mediatorDID = strings.TrimSpace(mediatorDID)
	if hostingDID == "" || mediatorDID == "" {
		return connectionTarget{}, http.StatusBadRequest, errors.New("did_hosting_did and mediator_did are both required")
	}

	var matches []model.SetupSession
	if err := h.db.Where("mode = ? AND did_hosting_did = ?", model.ModeFullStack, hostingDID).Find(&matches).Error; err != nil {
		return connectionTarget{}, http.StatusInternalServerError, errors.New("failed to check stack DIDs")
	}
	if len(matches) > 0 {
		if len(matches) != 1 {
			return connectionTarget{}, http.StatusConflict, errors.New("DID hosting DID matches multiple stacks on this farm")
		}
		stack := &matches[0]
		target, err := farmConnectionTarget(stack, mediatorDID)
		if err != nil {
			return connectionTarget{}, http.StatusConflict, err
		}
		if status, err := h.validateMediatorDID(ctx, mediatorDID); err != nil {
			return connectionTarget{}, status, err
		}
		if h.didHosting == nil {
			return connectionTarget{}, http.StatusServiceUnavailable, errors.New("automatic DID publication is not configured on this farm")
		}
		return target, http.StatusOK, nil
	}

	baseURL, err := webvh.HostingBaseURL(hostingDID)
	if err != nil {
		return connectionTarget{}, http.StatusBadRequest, fmt.Errorf("invalid did_hosting_did: %w", err)
	}
	resolver := webvh.NewResolver(5 * time.Second)
	if err := resolver.ResolveDID(ctx, hostingDID); err != nil {
		return connectionTarget{}, http.StatusUnprocessableEntity, fmt.Errorf("DID hosting DID does not resolve: %w", err)
	}
	if status, err := h.validateMediatorDID(ctx, mediatorDID); err != nil {
		return connectionTarget{}, status, err
	}
	return connectionTarget{
		infra: sharedInfra{
			MediatorDid: mediatorDID,
			ServerURL:   baseURL,
			DaemonDid:   hostingDID,
		},
		source: model.ConnectionExternal,
	}, http.StatusOK, nil
}

func farmConnectionTarget(stack *model.SetupSession, mediatorDID string) (connectionTarget, error) {
	if stack.Status != "running" || stack.DidsSubdomain == "" || stack.Domain == "" || stack.DIDHostingDid == "" {
		return connectionTarget{}, errors.New("this farm's DID host is not ready")
	}
	source := model.ConnectionInFarm
	var providerID *uint
	if stack.DomainType == model.DomainPlatform {
		source = model.ConnectionPlatform
	} else {
		providerID = &stack.ID
	}
	infra := sharedInfra{MediatorDid: mediatorDID, ServerURL: stack.DidsURL(), ControlURL: stack.DidsURL(), DaemonDid: stack.DIDHostingDid}
	return connectionTarget{infra: infra, source: source, providerID: providerID, providerName: stack.VtaName}, nil
}

func (h *SetupHandler) validateMediatorDID(ctx context.Context, did string) (int, error) {
	var stacks []model.SetupSession
	if err := h.db.Where("mode = ? AND mediator_did = ?", model.ModeFullStack, did).Find(&stacks).Error; err != nil {
		return http.StatusInternalServerError, errors.New("failed to check mediator DID")
	}
	if len(stacks) > 0 {
		for _, stack := range stacks {
			if stack.Status == "running" {
				return http.StatusOK, nil
			}
		}
		return http.StatusConflict, errors.New("this farm's mediator is not running")
	}
	if err := webvh.NewResolver(5*time.Second).ResolveDID(ctx, did); err != nil {
		return http.StatusUnprocessableEntity, fmt.Errorf("mediator DID does not resolve: %w", err)
	}
	return http.StatusOK, nil
}

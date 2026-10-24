package setup

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/vta"
	"github.com/ic3software/vtafarm-api/internal/webvh"
)

const (
	// referralGrantLabel names the ACL entry the referral edit runs under.
	referralGrantLabel = "trust-registry-referral"
	// referralGrantTTL bounds that entry; the VTA's sweeper removes it. The
	// key itself lives only in this process, for the length of one call.
	referralGrantTTL = "15m"
	// referralHostWait is how long to give the restarted VTA to bring its
	// mediator session up before the edit, whose publish needs it.
	referralHostWait = 3 * time.Minute
	// referralDidsServer is the id the session's DID hosting daemon is
	// registered under at the VTA (`vta did-mgmt servers add --id dids`).
	referralDidsServer = "dids"
)

// newVtaClient is the seam a test replaces to stand in a VTA.
var newVtaClient = vta.NewEphemeral

// publishRegistryReferral adds the TrustRegistry referral to the VTC's
// published DID document, through the running VTA.
//
// Why the running VTA: the offline `vta did-mgmt dids edit` this step used to
// run holds no mediator session, so it can publish only to a DID host that
// advertises an HTTPS endpoint. A host provisioned through a VTA with a
// mediator, which is every Full Stack `dids` daemon, advertises TSP and
// DIDComm alone, and the offline edit stopped with `no transport in common`
// after committing the entry locally. The running VTA holds the mediator
// session, publishes over either, and re-publishes an unconfirmed local head
// before applying the next update, which is also how a stack left behind by
// that offline attempt converges.
//
// The farm has no standing credential at a user's VTA, and does not want one.
// It mints a did:key for this call, grants it as an admin of the VTC's context
// for fifteen minutes in the same stopped-VTA window pattern the registry context
// grant uses, logs in over HTTPS, and sends the update signed by that key. On
// return the key is garbage; the entry expires on its own.
//
// Idempotent by observation, not by record: a published document that
// already carries the referral means nothing to do, with no grant and no
// restart; a VTA head that carries it while the host does not gets an update
// with an unchanged document, which the VTA answers by reconciling the host
// and then appending a version. Either way the verify phase that follows reads
// the host, not this function's belief.
func (o *Orchestrator) publishRegistryReferral(ctx context.Context, ns string, s *model.SetupSession) error {
	published, err := webvh.NewResolver(webvh.DefaultTimeout).ResolveDocument(ctx, s.VtcDid)
	if err != nil {
		return fmt.Errorf("resolve the VTC DID before publishing the referral: %w", err)
	}
	if _, changed, err := RegistryReferral(published, s.VtcDid, s.TrustRegistryDid); err != nil {
		return err
	} else if !changed {
		log.Printf("[orchestrator] fs session %d: %s already refers to the registry; nothing to publish", s.ID, s.VtcDid)
		return nil
	}

	client, err := newVtaClient(s.PublicURL(), s.VtaDid)
	if err != nil {
		return err
	}
	grant := []string{"vta", "acl", "create", "--did", client.DID(), "--role", "admin", "--contexts", s.VtcName, "--expires", referralGrantTTL, "--label", referralGrantLabel}
	err = o.registryStopped(ctx, ns, s, "vta", func() error {
		_, e := o.registryJob(ctx, ns, s, "referral", s.VtaImage, "/work/vta", grant, []k8s.PVCMount{{Name: "vta-data", ClaimName: k8s.FSVtaName(s.ID), MountPath: "/work/vta"}}, nil)
		return e
	})
	if err != nil {
		return err
	}

	if err = client.Login(ctx); err != nil {
		return fmt.Errorf("log in to the VTA as the referral grant %s: %w", client.DID(), err)
	}
	if err = client.WaitUntilHostReachable(ctx, referralDidsServer, referralHostWait); err != nil {
		return fmt.Errorf("the restarted VTA has no route to the DID host, so it could not publish: %w", err)
	}
	didLog, err := client.DIDLog(ctx, s.VtcDid)
	if err != nil {
		return err
	}
	head, state, err := vta.LatestLogEntry(didLog)
	if err != nil {
		return fmt.Errorf("read the VTA's head of %s: %w", s.VtcDid, err)
	}
	updated, changed, err := RegistryReferral(state, s.VtcDid, s.TrustRegistryDid)
	if err != nil {
		return err
	}
	if !changed {
		// The VTA's head has the referral and the host does not: an earlier
		// publish failed after the local commit. An update that changes
		// nothing in the document still makes the VTA re-publish that head
		// first, then append a version the host takes.
		log.Printf("[orchestrator] fs session %d: the VTA's head of %s (%s) carries the referral but the host does not; publishing through a new version", s.ID, s.VtcDid, head)
		updated = state
	}
	newVersion, err := client.UpdateDIDDocument(ctx, s.VtcDid, updated, head)
	if err != nil {
		var refusal *vta.TaskError
		if errors.As(err, &refusal) && refusal.Conflict() {
			return fmt.Errorf("the VTC DID advanced past %s while the referral was being published; retry registry enablement: %w", head, err)
		}
		return err
	}
	log.Printf("[orchestrator] fs session %d: TrustRegistry referral published; %s is now version %s", s.ID, s.VtcDid, newVersion)
	return nil
}

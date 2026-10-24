package vault

import (
	"context"
	"fmt"
)

func TrustRegistryPrefix(userID, sessionID uint) string {
	return fmt.Sprintf("trust-registry/user-%d/session-%d", userID, sessionID)
}

func (c *Client) DeleteTrustRegistrySecrets(ctx context.Context, userID, sessionID uint) error {
	token, err := c.login(ctx)
	if err != nil {
		return err
	}
	return c.deleteSecretTree(ctx, token, TrustRegistryPrefix(userID, sessionID))
}

// Package privatebin provides a thin wrapper around the PrivateBin v2 API for
// posting the morning briefing HTML as an encrypted paste and returning the
// shareable URL (with decryption key in the fragment).
package privatebin

import (
	"context"
	"fmt"
	"net/url"

	pb "go.gearno.de/privatebin/v2"
)

// PostPaste encrypts htmlContent and posts it to the PrivateBin instance at
// instanceURL (e.g. "https://privatebin.net"). Returns the full paste URL
// including the decryption key fragment, or an error.
//
// Pastes are set to expire after 24 hours, use the "plaintext" formatter (so
// the HTML renders when opened), and have no password or burn-after-reading.
func PostPaste(ctx context.Context, instanceURL string, htmlContent []byte) (string, error) {
	endpoint, err := url.Parse(instanceURL)
	if err != nil {
		return "", fmt.Errorf("parse privatebin url %q: %w", instanceURL, err)
	}

	client := pb.NewClient(*endpoint)

	result, err := client.CreatePaste(ctx, htmlContent, pb.CreatePasteOptions{
		Formatter:        "plaintext",
		Expire:           "1day",
		OpenDiscussion:   false,
		BurnAfterReading: false,
		Compress:         pb.CompressionAlgorithmGZip,
	})
	if err != nil {
		return "", fmt.Errorf("create paste: %w", err)
	}

	return result.PasteURL.String(), nil
}

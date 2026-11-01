package creds

import (
	"fmt"
	"strings"
)

// S3-compatible storage. Drime is the primary destination; any storage that
// speaks S3 is the other kind, with presets for the common ones and a custom
// endpoint for the rest. Cloudflare R2 keeps its original provider value
// ("r2", or empty in files from before Drime) so nothing already saved
// changes meaning.
const (
	// ProviderB2 is Backblaze B2's S3-compatible API.
	ProviderB2 = "b2"
	// ProviderS3 is Amazon S3.
	ProviderS3 = "s3"
	// ProviderCustom is any other S3-compatible endpoint the person enters:
	// MinIO, Wasabi, a NAS.
	ProviderCustom = "s3-custom"
)

// S3Presets are the S3-compatible providers, in the order they are offered.
var S3Presets = []string{ProviderR2, ProviderB2, ProviderS3, ProviderCustom}

// Kind is the provider with the empty value read as R2.
func (c Credentials) Kind() string {
	if c.Provider == "" {
		return ProviderR2
	}
	return c.Provider
}

// IsS3 reports whether these are for S3-compatible storage of any kind.
func (c Credentials) IsS3() bool { return !c.IsDrime() }

// ProviderLabel names the provider as a person would write it.
func ProviderLabel(kind string) string {
	switch kind {
	case ProviderDrime:
		return "Drime"
	case ProviderB2:
		return "Backblaze B2"
	case ProviderS3:
		return "Amazon S3"
	case ProviderCustom:
		return "S3-compatible storage"
	default:
		return "Cloudflare R2"
	}
}

// Label is ProviderLabel for these credentials.
func (c Credentials) Label() string { return ProviderLabel(c.Kind()) }

// S3Endpoint is how to reach S3-compatible credentials: the endpoint URL
// ("" lets the SDK work it out from the region, for Amazon S3), the region
// to sign for, and whether the bucket goes in the hostname rather than the
// path.
type S3Endpoint struct {
	URL         string
	Region      string
	VirtualHost bool
}

// S3 works out where S3-compatible credentials point.
func (c Credentials) S3() S3Endpoint {
	switch c.Kind() {
	case ProviderB2:
		url := c.Endpoint
		if url == "" && c.Region != "" {
			url = "https://s3." + c.Region + ".backblazeb2.com"
		}
		return S3Endpoint{URL: url, Region: c.Region}
	case ProviderS3:
		region := c.Region
		if region == "" {
			region = "us-east-1"
		}
		return S3Endpoint{URL: c.Endpoint, Region: region, VirtualHost: true}
	case ProviderCustom:
		region := c.Region
		if region == "" {
			region = "us-east-1"
		}
		return S3Endpoint{URL: c.Endpoint, Region: region, VirtualHost: c.VirtualHost}
	default: // R2
		url := c.Endpoint
		if url == "" && c.AccountID != "" {
			url = fmt.Sprintf("https://%s.r2.cloudflarestorage.com", c.AccountID)
		}
		return S3Endpoint{URL: url, Region: "auto"}
	}
}

// validS3 checks S3-compatible credentials beyond the key pair and bucket.
func (c Credentials) validS3() []string {
	var missing []string
	switch c.Kind() {
	case ProviderR2:
		if c.AccountID == "" && c.Endpoint == "" {
			missing = append(missing, "account id")
		}
	case ProviderB2:
		if c.Region == "" && c.Endpoint == "" {
			missing = append(missing, "region (us-west-004, say)")
		}
	case ProviderS3:
	case ProviderCustom:
		if c.Endpoint == "" {
			missing = append(missing, "endpoint URL")
		} else if !strings.HasPrefix(c.Endpoint, "https://") && !strings.HasPrefix(c.Endpoint, "http://") {
			missing = append(missing, "an endpoint starting with https://")
		}
	}
	return missing
}

// knownProvider reports whether p is a provider this build understands.
func knownProvider(p string) bool {
	switch p {
	case "", ProviderR2, ProviderDrime, ProviderB2, ProviderS3, ProviderCustom:
		return true
	}
	return false
}

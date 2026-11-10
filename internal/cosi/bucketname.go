package cosi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

const (
	minimumBucketNameLength = 3
	maximumBucketNameLength = 63
	// bucketNameHashLength is how many hex characters of the name's hash end a shortened bucket name.
	bucketNameHashLength = 10
	// maximumAccountPrefixLength leaves room after the account's bucket prefix for the hash of a shortened name.
	maximumAccountPrefixLength = maximumBucketNameLength - bucketNameHashLength - 1
	// maximumClassPrefixLength bounds bucketNamePrefix, so the COSI bucket name still shows in the bucket name.
	maximumClassPrefixLength = 24
)

var (
	accountPrefixPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)
	classPrefixPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	ipAddressPattern     = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$`)
)

// BucketName derives the S3 bucket name of a COSI bucket: the account's bucket prefix (required on single-node zones),
// then the BucketClass's bucketNamePrefix, then the COSI bucket name (see sanitizeBucketNamePart). Without either
// prefix, a COSI bucket name that is a valid bucket name stays as it is, so it matches the bucket name the provisioner
// sidecar writes into a BucketAccess's Secret. A name that would be longer than 63 characters, or shorter than 3,
// keeps as much of its start as fits and ends in a hash of all three inputs, so it stays unique; the same inputs
// always derive the same name, which is what makes a retried DriverCreateBucket find its bucket.
func BucketName(accountPrefix string, classPrefix string, cosiName string) (string, error) {
	if accountPrefix != "" && (!accountPrefixPattern.MatchString(accountPrefix) || len(accountPrefix) > maximumAccountPrefixLength) {
		return "", fmt.Errorf("the account's bucket prefix %q cannot start an S3 bucket name", accountPrefix)
	}
	body := joinNonEmpty(sanitizeBucketNamePart(classPrefix), sanitizeBucketNamePart(cosiName))
	name := accountPrefix + body
	if body != "" && len(name) >= minimumBucketNameLength && len(name) <= maximumBucketNameLength {
		return name, nil
	}
	digest := sha256.Sum256([]byte(accountPrefix + "\x00" + classPrefix + "\x00" + cosiName))
	suffix := hex.EncodeToString(digest[:])[:bucketNameHashLength]
	keep := maximumBucketNameLength - bucketNameHashLength - 1
	if len(name) > keep {
		name = name[:keep]
	}
	name = strings.TrimRight(name, "-.")
	if name == "" {
		return suffix, nil
	}
	return name + "-" + suffix, nil
}

// ValidateClassPrefix checks a BucketClass's bucketNamePrefix: 1 to 24 characters of [a-z0-9-], starting with a
// letter or digit.
func ValidateClassPrefix(classPrefix string) error {
	if len(classPrefix) > maximumClassPrefixLength || !classPrefixPattern.MatchString(classPrefix) {
		return fmt.Errorf("%s %q must be 1 to %d characters of a-z, 0-9 and '-', starting with a letter or digit",
			ParameterBucketNamePrefix, classPrefix, maximumClassPrefixLength)
	}
	return nil
}

// BucketID is the COSI bucket id of a bucket: `<zone>/<bucket name>`.
func BucketID(zone string, bucketName string) string {
	return zone + "/" + bucketName
}

// ParseBucketID splits a bucket id into its zone and bucket name.
func ParseBucketID(bucketID string) (string, string, error) {
	zone, bucketName, isSplit := strings.Cut(bucketID, "/")
	if !isSplit || zone == "" || bucketName == "" || strings.Contains(bucketName, "/") {
		return "", "", fmt.Errorf("bucket id %q is not <zone>/<bucket name>", bucketID)
	}
	return zone, bucketName, nil
}

// sanitizeBucketNamePart lowercases part and keeps a-z, 0-9 and single dots between them; every other run of
// characters becomes one '-', and separators at either end are dropped. A Kubernetes object name (a DNS subdomain)
// comes through unchanged, unless it reads as an IPv4 address, which S3 refuses as a bucket name.
func sanitizeBucketNamePart(part string) string {
	var builder strings.Builder
	pendingSeparator := ""
	for _, character := range strings.ToLower(part) {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') {
			if pendingSeparator != "" && builder.Len() > 0 {
				builder.WriteString(pendingSeparator)
			}
			pendingSeparator = ""
			builder.WriteRune(character)
			continue
		}
		if pendingSeparator == "" && character == '.' {
			pendingSeparator = "."
		} else {
			pendingSeparator = "-"
		}
	}
	sanitized := builder.String()
	if ipAddressPattern.MatchString(sanitized) {
		sanitized = strings.ReplaceAll(sanitized, ".", "-")
	}
	return sanitized
}

func joinNonEmpty(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "-")
}

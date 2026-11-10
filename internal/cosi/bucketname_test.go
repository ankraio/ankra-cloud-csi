package cosi

import (
	"regexp"
	"strings"
	"testing"
)

var validBucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

func TestBucketNameKeepsAValidNameAsIs(t *testing.T) {
	name, nameError := BucketName("", "", "objects-0f4c2a8e-1b2c-4d5e-8f90-123456789abc")
	if nameError != nil || name != "objects-0f4c2a8e-1b2c-4d5e-8f90-123456789abc" {
		t.Fatalf("BucketName = %q, %v", name, nameError)
	}
}

func TestBucketNameKeepsAKubernetesObjectNameSoItMatchesTheSidecarsBucketInfo(t *testing.T) {
	cosiName := "team.objects-0f4c2a8e-1b2c-4d5e-8f90-123456789abc"
	name, nameError := BucketName("", "", cosiName)
	if nameError != nil || name != cosiName {
		t.Fatalf("BucketName = %q, %v; want the COSI name unchanged", name, nameError)
	}
}

func TestBucketNameLowercasesAndReplacesInvalidCharacters(t *testing.T) {
	cases := map[string]string{
		"My_Bucket.Class":   "my-bucket.class",
		"--Leading__and__":  "leading-and",
		"a..b//c  d":        "a-b-c-d",
		".dots.-.mixed.":    "dots-mixed",
		"Ünïcode-bücket-01": "n-code-b-cket-01",
		"10.0.0.1":          "10-0-0-1",
	}
	for input, want := range cases {
		name, nameError := BucketName("", "", input)
		if nameError != nil || name != want {
			t.Errorf("BucketName(%q) = %q, %v; want %q", input, name, nameError, want)
		}
		if !validBucketName.MatchString(name) {
			t.Errorf("BucketName(%q) = %q is not a valid bucket name", input, name)
		}
	}
}

func TestBucketNamePutsTheAccountPrefixFirstThenTheClassPrefix(t *testing.T) {
	name, nameError := BucketName("a1b2c3-", "team-a", "Objects-1")
	if nameError != nil || name != "a1b2c3-team-a-objects-1" {
		t.Fatalf("BucketName = %q, %v", name, nameError)
	}
	withoutClass, _ := BucketName("a1b2c3-", "", "objects-1")
	if withoutClass != "a1b2c3-objects-1" {
		t.Fatalf("BucketName without a class prefix = %q", withoutClass)
	}
}

func TestBucketNameShortensLongNamesWithAHashAndStaysDeterministic(t *testing.T) {
	long := "a-very-long-bucket-class-name-" + strings.Repeat("x", 40) + "-0f4c2a8e-1b2c-4d5e-8f90-123456789abc"
	first, firstError := BucketName("a1b2c3-", "team", long)
	second, secondError := BucketName("a1b2c3-", "team", long)
	if firstError != nil || secondError != nil {
		t.Fatalf("BucketName: %v %v", firstError, secondError)
	}
	if first != second {
		t.Fatalf("BucketName is not deterministic: %q and %q", first, second)
	}
	if len(first) > maximumBucketNameLength || !strings.HasPrefix(first, "a1b2c3-team-") || !validBucketName.MatchString(first) {
		t.Fatalf("BucketName = %q (%d characters)", first, len(first))
	}
	other, _ := BucketName("a1b2c3-", "team", long+"-other")
	if other == first {
		t.Fatalf("two long names that share their start derived the same bucket %q", first)
	}
}

func TestBucketNamePadsShortAndEmptyNames(t *testing.T) {
	for _, input := range []string{"a", "ab", "", "___"} {
		name, nameError := BucketName("", "", input)
		if nameError != nil || len(name) < minimumBucketNameLength || !validBucketName.MatchString(name) {
			t.Errorf("BucketName(%q) = %q, %v", input, name, nameError)
		}
	}
	prefixed, _ := BucketName("a1b2c3-", "", "")
	if !strings.HasPrefix(prefixed, "a1b2c3-") || !validBucketName.MatchString(prefixed) {
		t.Fatalf("BucketName with only the account prefix = %q", prefixed)
	}
}

func TestBucketNameRefusesAnAccountPrefixThatCannotStartABucketName(t *testing.T) {
	for _, prefix := range []string{"Upper-", "-dash", strings.Repeat("a", maximumAccountPrefixLength+1)} {
		if _, nameError := BucketName(prefix, "", "objects"); nameError == nil {
			t.Errorf("BucketName accepted the account prefix %q", prefix)
		}
	}
}

func TestValidateClassPrefix(t *testing.T) {
	for _, valid := range []string{"team-a", "a", "0abc"} {
		if prefixError := ValidateClassPrefix(valid); prefixError != nil {
			t.Errorf("ValidateClassPrefix(%q): %v", valid, prefixError)
		}
	}
	for _, invalid := range []string{"Team", "-team", "team_a", "team.a", strings.Repeat("a", maximumClassPrefixLength+1)} {
		if ValidateClassPrefix(invalid) == nil {
			t.Errorf("ValidateClassPrefix accepted %q", invalid)
		}
	}
}

func TestParseBucketID(t *testing.T) {
	zone, bucketName, parseError := ParseBucketID(BucketID("fsn1", "a1b2c3-objects"))
	if parseError != nil || zone != "fsn1" || bucketName != "a1b2c3-objects" {
		t.Fatalf("ParseBucketID = %q %q %v", zone, bucketName, parseError)
	}
	for _, invalid := range []string{"", "fsn1", "/bucket", "fsn1/", "fsn1/a/b"} {
		if _, _, invalidError := ParseBucketID(invalid); invalidError == nil {
			t.Errorf("ParseBucketID accepted %q", invalid)
		}
	}
}

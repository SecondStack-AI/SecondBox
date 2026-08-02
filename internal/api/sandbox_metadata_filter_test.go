package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func metadataFilterRequest(t *testing.T, query url.Values) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, "https://secondbox.example/v1/sandboxes", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.URL.RawQuery = query.Encode()
	return request
}

func TestQueryMetadataFilterParsesRepeatedPairs(t *testing.T) {
	filter, err := queryMetadataFilter(metadataFilterRequest(t, url.Values{
		"metadata": []string{"secondbox.dev/name=my-box", "tier=gold"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(filter) != 2 ||
		filter["secondbox.dev/name"] != "my-box" || filter["tier"] != "gold" {
		t.Fatalf("filter = %#v", filter)
	}
}

func TestQueryMetadataFilterIsAbsentByDefault(t *testing.T) {
	filter, err := queryMetadataFilter(metadataFilterRequest(t, url.Values{
		"limit": []string{"10"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if filter != nil {
		t.Fatalf("filter = %#v; want none", filter)
	}
}

// TestQueryMetadataFilterSplitsOnTheFirstSeparator proves a value may itself
// contain '=', which base64 and query-string values routinely do.
func TestQueryMetadataFilterSplitsOnTheFirstSeparator(t *testing.T) {
	filter, err := queryMetadataFilter(metadataFilterRequest(t, url.Values{
		"metadata": []string{"token=a=b=c"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if filter["token"] != "a=b=c" {
		t.Fatalf("filter = %#v; want the full value preserved", filter)
	}
}

func TestQueryMetadataFilterAcceptsAnEmptyValue(t *testing.T) {
	filter, err := queryMetadataFilter(metadataFilterRequest(t, url.Values{
		"metadata": []string{"tier="},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if value, present := filter["tier"]; !present || value != "" {
		t.Fatalf("filter = %#v; want an empty value to be preserved", filter)
	}
}

func TestQueryMetadataFilterRejectsMalformedEntries(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		wantIn string
	}{
		{"no separator", []string{"tier"}, "name=value"},
		{"blank name", []string{"=gold"}, "name=value"},
		{"whitespace name", []string{"   =gold"}, "name=value"},
		{"name too long", []string{strings.Repeat("a", 129) + "=gold"}, "name=value"},
		{"value too long", []string{"tier=" + strings.Repeat("a", 1025)}, "name=value"},
		{"repeated name", []string{"tier=gold", "tier=silver"}, "must not repeat"},
		{
			"too many entries",
			[]string{"a=1", "b=2", "c=3", "d=4", "e=5", "f=6", "g=7", "h=8", "i=9"},
			"must not exceed 8 entries",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := queryMetadataFilter(metadataFilterRequest(t, url.Values{
				"metadata": test.values,
			}))
			if err == nil || !strings.Contains(err.Error(), test.wantIn) {
				t.Fatalf("error = %v; want one containing %q", err, test.wantIn)
			}
		})
	}
}

func TestQueryMetadataFilterAcceptsTheBoundaryLengths(t *testing.T) {
	name := strings.Repeat("a", 128)
	value := strings.Repeat("b", 1024)
	filter, err := queryMetadataFilter(metadataFilterRequest(t, url.Values{
		"metadata": []string{name + "=" + value},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if filter[name] != value {
		t.Fatal("the maximum permitted name and value must be accepted")
	}
}

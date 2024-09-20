package detect

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/spf13/viper"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/report"
)

func TestVerify(t *testing.T) {
	// Create an httptest.Server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Inspect the request
		switch r.URL.Path {
		case "/valid":
			// Check headers
			if r.Header.Get("Authorization") == "Bearer validtoken" {
				// Return 200 OK
				w.WriteHeader(200)
				w.Write([]byte(`{"status": "success"}`))
			} else {
				// Return 401 Unauthorized
				w.WriteHeader(401)
				w.Write([]byte(`{"status": "unauthorized"}`))
			}
		case "/invalid":
			w.WriteHeader(401)
			w.Write([]byte(`{"status": "unauthorized"}`))
		default:
			// Return 404 Not Found
			w.WriteHeader(404)
			w.Write([]byte("Not Found"))
		}
	}))
	defer server.Close()

	// Initialize Detector with the server's client
	detector := &Detector{
		HTTPClient:  server.Client(), // Use the server's client
		VerifyCache: *NewRequestCache(),
	}

	tests := []struct {
		name      string
		findings  []report.Finding
		configStr string
		want      []report.Finding
	}{
		{
			name: "Valid token with correct header",
			findings: []report.Finding{
				{
					RuleID: "TokenRule",
					Secret: "validtoken",
				},
			},
			configStr: fmt.Sprintf(`
                [[rules]]
                id = "TokenRule"
                regex = '''(?i)\b([a-z0-9]{10})\b'''
                report = false
                [rules.verify]
                url = "%s/valid"
                httpVerb = "GET"
                expectedStatus = ["200"]
                headers = {Authorization = "Bearer ${TokenRule}"}
            `, server.URL),
			want: []report.Finding{
				{
					RuleID:       "TokenRule",
					Secret:       "validtoken",
					Status:       report.ConfirmedValid,
					StatusReason: "",
				},
			},
		},
		{
			name: "Invalid token",
			findings: []report.Finding{
				{
					RuleID: "TokenRule",
					Secret: "invalidtoken",
				},
			},
			configStr: fmt.Sprintf(`
                [[rules]]
                id = "TokenRule"
                regex = '''(?i)\b([a-z0-9]{12})\b'''
                report = false
                [rules.verify]
                url = "%s/valid"
                httpVerb = "GET"
                expectedStatus = ["200"]
                headers = {Authorization = "Bearer ${TokenRule}"}
            `, server.URL),
			want: []report.Finding{
				{
					RuleID:       "TokenRule",
					Secret:       "invalidtoken",
					Status:       report.ConfirmedInvalid,
					StatusReason: "Status code '401'",
				},
			},
		},
		{
			name: "Invalid URL path",
			findings: []report.Finding{
				{
					RuleID: "TokenRule",
					Secret: "validtoken",
				},
			},
			configStr: fmt.Sprintf(`
                [[rules]]
                id = "TokenRule"
                regex = '''(?i)\b([a-z0-9]{10})\b'''
                report = false
                [rules.verify]
                url = "%s/invalid_path"
                httpVerb = "GET"
                expectedStatus = ["200"]
                headers = {Authorization = "Bearer ${TokenRule}"}
            `, server.URL),
			want: []report.Finding{
				{
					RuleID:       "TokenRule",
					Secret:       "validtoken",
					Status:       report.ConfirmedInvalid,
					StatusReason: "Status code '404'",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Load the config from tt.configStr
			viper.Reset()
			viper.SetConfigType("toml")
			err := viper.ReadConfig(strings.NewReader(tt.configStr))
			require.NoError(t, err)

			var vc config.ViperConfig
			err = viper.Unmarshal(&vc)
			require.NoError(t, err)
			cfg, err := vc.Translate()
			require.NoError(t, err)

			detector.Config = cfg

			verifiedFindings := detector.Verify(tt.findings)
			// Compare findings while ignoring unexported fields and order
			assert.Equal(t, tt.want, verifiedFindings)
		})
	}
}

func Test_expandPlaceholdersInString(t *testing.T) {
	type args struct {
		template                string
		placeholder             string
		secret                  string
		placeholderByRequiredID map[string]string
		secretsByRequiredID     map[string][]string
	}
	tests := []struct {
		name string
		args args
		want []string
	}{
		// This should never happen.
		{
			name: "no placeholders",
			args: args{
				template: "https://example.com/foo?bar=baz",
			},
			want: []string{"https://example.com/foo?bar=baz"},
		},
		{
			name: "one placeholder, one finding",
			args: args{
				template:    "https://example.com/foo?key=${rule-id}",
				placeholder: "${rule-id}",
				secret:      "s3cr3t",
			},
			want: []string{"https://example.com/foo?key=s3cr3t"},
		},
		{
			name: "one placeholder, many findings",
			args: args{
				template:    "https://example.com/foo?key=${rule-id}",
				placeholder: "${rule-id}",
				secret:      "s3cr3t",
				// These shouldn't be used.
				placeholderByRequiredID: map[string]string{
					"rule-id": "${rule-id}",
				},
				secretsByRequiredID: map[string][]string{
					"rule-id": {"changeme"},
				},
			},
			want: []string{"https://example.com/foo?key=s3cr3t"},
		},
		{
			name: "many placeholders, one finding",
			args: args{
				template:    "https://example.com/foo?key-id=${id-rule}&key-secret=${secret-rule}",
				placeholder: "${id-rule}",
				secret:      "gitleaks",
				placeholderByRequiredID: map[string]string{
					"secret-rule": "${secret-rule}",
				},
				secretsByRequiredID: map[string][]string{
					"secret-rule": {"s3cr3t"},
				},
			},
			want: []string{"https://example.com/foo?key-id=gitleaks&key-secret=s3cr3t"},
		},
		{
			name: "many placeholders, many findings",
			args: args{
				template:    "https://example.com/foo?key-id=${id-rule}&key-secret=${secret-rule}",
				placeholder: "${id-rule}",
				secret:      "gitleaks",
				placeholderByRequiredID: map[string]string{
					"secret-rule": "${secret-rule}",
				},
				secretsByRequiredID: map[string][]string{
					"secret-rule": {"s3cr3t-1", "s3cr3t_2"},
				},
			},
			want: []string{
				"https://example.com/foo?key-id=gitleaks&key-secret=s3cr3t-1",
				"https://example.com/foo?key-id=gitleaks&key-secret=s3cr3t_2",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := expandPlaceholdersInString(tt.args.template, tt.args.placeholder, tt.args.secret, tt.args.placeholderByRequiredID, tt.args.secretsByRequiredID)
			// https://stackoverflow.com/a/67624073
			less := func(a, b string) bool { return a < b }
			if diff := cmp.Diff(tt.want, actual, cmpopts.SortSlices(less)); diff != "" {
				t.Errorf("diff: (-want +got)\n%s", diff)
			}
		})
	}
}
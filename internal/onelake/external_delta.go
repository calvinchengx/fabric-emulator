package onelake

// A shortcut's Delta table, read directly rather than through the DFS surface's
// one-file-at-a-time shape — what the warehouse reflector needs to serve an
// ADLS Gen2, Amazon S3 or Dataverse shortcut as a table on a lakehouse's SQL
// analytics endpoint (docs/61), the same way it already serves a OneLake one.
//
// "Shortcuts function as tables in the SQL analytics endpoint" does not say
// which shortcut kinds; OneLake ones already do (#526). This is the other
// three, over the same read-through this package already has for a single
// file — the missing piece is LISTING a directory on someone else's storage,
// which resolveExternal never needed.

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/awssig"
	"github.com/calvinchengx/fabric-emulator/internal/httpx"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// ExternalDeltaCommits lists an external shortcut's _delta_log JSON commit
// filenames, sorted — the same question activeFiles asks of a OneLake-native
// table, asked of someone else's storage. Only the immediate children: this
// repository's Delta reader supports no checkpoints, so a nested object would
// not be a commit it could read anyway, and a truncated listing is refused
// rather than silently under-read (listS3, listBlob).
func (s *Service) ExternalDeltaCommits(sc *store.Shortcut) ([]string, error) {
	prefix := joinPath(joinPath(sc.TargetPath, sc.TargetTable), "_delta_log") + "/"
	names, err := s.listExternal(sc, prefix)
	if err != nil {
		return nil, fmt.Errorf("listing %s's _delta_log: %w", sc.Name, err)
	}
	var out []string
	for _, n := range names {
		rel := strings.TrimPrefix(n, prefix)
		if rel == "" || strings.Contains(rel, "/") || !strings.HasSuffix(rel, ".json") {
			continue // the folder itself, a nested object, or not a commit
		}
		out = append(out, rel)
	}
	sort.Strings(out) // 000..0.json ordering is lexicographic
	return out, nil
}

// ExternalReadFile reads one file under an external shortcut's root — the same
// read resolveExternal serves over the DFS surface, returned as plain bytes for
// a caller (the warehouse reflector) that is not itself a DFS request.
func (s *Service) ExternalReadFile(sc *store.Shortcut, remainder string) ([]byte, error) {
	p, derr := s.resolveExternal(sc, remainder)
	if derr != nil {
		return nil, fmt.Errorf("%s: %s", derr.code, derr.msg)
	}
	return p.Content, nil
}

// listExternal lists object/blob names whose key starts with prefix, under the
// shortcut's target. Dispatches on target type because S3 and Azure Blob
// storage each have their own listing call, with their own signing.
func (s *Service) listExternal(sc *store.Shortcut, prefix string) ([]string, error) {
	conn, err := s.Store.GetConnection(sc.ConnectionID)
	if err != nil {
		return nil, fmt.Errorf("resolving the shortcut connection: %w", err)
	}
	var creds struct {
		CredentialType, Username, Password, Token string
	}
	_ = json.Unmarshal([]byte(conn.CredentialsJSON), &creds)

	switch sc.TargetType {
	case "AmazonS3":
		if creds.CredentialType != "Basic" {
			return nil, fmt.Errorf("an Amazon S3 shortcut needs a Basic (access key) credential to list, not %q", creds.CredentialType)
		}
		return s.listS3(sc, prefix, creds.Username, creds.Password)
	default:
		// ADLSGen2 and Dataverse: the Blob "List Blobs" API — the endpoint this
		// repository's own coverage is against (docs/61: Azurite implements Blob,
		// not the DFS surface a real ADLS Gen2 shortcut's URL names).
		return s.listBlob(sc, prefix, creds)
	}
}

// listS3 lists an Amazon S3 bucket with ListObjectsV2, signed with SigV4 —
// GET {TargetLocation}/?list-type=2&prefix=…&delimiter=/, the same host and
// signing externalRequest uses for a single object, the bucket root instead of
// one key.
func (s *Service) listS3(sc *store.Shortcut, prefix, accessKeyID, secretAccessKey string) ([]string, error) {
	target, err := url.Parse(sc.TargetLocation + "/")
	if err != nil {
		return nil, err
	}
	q := url.Values{"list-type": {"2"}, "prefix": {prefix}, "delimiter": {"/"}}
	target.RawQuery = q.Encode()
	req, err := http.NewRequest(http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	awssig.Sign(req, awssig.Credentials{AccessKeyID: accessKeyID, SecretAccessKey: secretAccessKey},
		s.S3Region(), "s3", awssig.EmptyPayloadHash, time.Unix(s.Store.Now(), 0).UTC())
	body, err := s.doList(req)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Contents    []struct{ Key string } `xml:"Contents"`
		IsTruncated bool                   `xml:"IsTruncated"`
	}
	if err := xml.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parsing the S3 ListObjectsV2 response: %w", err)
	}
	if parsed.IsTruncated {
		return nil, fmt.Errorf("more than one page of objects under %q; pagination is not supported", prefix)
	}
	names := make([]string, len(parsed.Contents))
	for i, c := range parsed.Contents {
		names[i] = c.Key
	}
	return names, nil
}

// listBlob lists an Azure Blob container with the List Blobs API —
// GET {TargetLocation}?restype=container&comp=list&prefix=…, the SAS token (if
// any) applied as query parameters, the same way a read applies it.
func (s *Service) listBlob(sc *store.Shortcut, prefix string, creds struct{ CredentialType, Username, Password, Token string }) ([]string, error) {
	target, err := url.Parse(sc.TargetLocation)
	if err != nil {
		return nil, err
	}
	q := url.Values{"restype": {"container"}, "comp": {"list"}, "prefix": {prefix}}
	switch creds.CredentialType {
	case "SharedAccessSignature":
		sasQ, err := url.ParseQuery(strings.TrimPrefix(creds.Token, "?"))
		if err != nil {
			return nil, fmt.Errorf("parsing the shortcut's SAS token: %w", err)
		}
		for k, vs := range sasQ {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
	case "", "Anonymous", "Basic":
		// Basic on a Blob-shaped target has no listing equivalent (the write path
		// does not use it either); anonymous containers need nothing added.
	default:
		return nil, fmt.Errorf("credential type %q cannot list a Blob container", creds.CredentialType)
	}
	target.RawQuery = q.Encode()
	body, err := s.doList(&http.Request{Method: http.MethodGet, URL: target})
	if err != nil {
		return nil, err
	}
	// NextMarker is a sibling of Blobs in the real response
	// (<EnumerationResults><Blobs>…</Blobs><NextMarker>…</NextMarker></EnumerationResults>),
	// not nested inside it.
	var parsed struct {
		Blobs struct {
			Blob []struct{ Name string } `xml:"Blob"`
		} `xml:"Blobs"`
		NextMarker string `xml:"NextMarker"`
	}
	if err := xml.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parsing the Blob List Blobs response: %w", err)
	}
	if parsed.NextMarker != "" {
		return nil, fmt.Errorf("more than one page of blobs under %q; pagination is not supported", prefix)
	}
	names := make([]string, len(parsed.Blobs.Blob))
	for i, b := range parsed.Blobs.Blob {
		names[i] = b.Name
	}
	return names, nil
}

// doList issues a listing GET and returns its body, refusing anything but 200 —
// a listing that failed must not be read as an empty one.
func (s *Service) doList(req *http.Request) ([]byte, error) {
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching the shortcut target: %w", err)
	}
	defer resp.Body.Close()
	body, ok := httpx.ReadBounded(resp.Body, httpx.MaxExternalRead)
	if !ok {
		return nil, fmt.Errorf("the listing response was larger than %d bytes, or the read failed; refusing to read a partial listing", int64(httpx.MaxExternalRead))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing returned HTTP %d: %s", resp.StatusCode, body)
	}
	return body, nil
}

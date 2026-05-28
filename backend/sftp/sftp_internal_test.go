//go:build !plan9

package sftp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/cache"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fstest/fstests"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

func TestShellEscapeUnix(t *testing.T) {
	for i, test := range []struct {
		unescaped, escaped string
	}{
		{"", ""},
		{"/this/is/harmless", "/this/is/harmless"},
		{"$(rm -rf /)", "\\$\\(rm\\ -rf\\ /\\)"},
		{"/test/\n", "/test/'\n'"},
		{":\"'", ":\\\"\\'"},
	} {
		got, err := quoteOrEscapeShellPath("unix", test.unescaped)
		assert.NoError(t, err)
		assert.Equal(t, test.escaped, got, fmt.Sprintf("Test %d unescaped = %q", i, test.unescaped))
	}
}

func TestShellEscapeCmd(t *testing.T) {
	for i, test := range []struct {
		unescaped, escaped string
		ok                 bool
	}{
		{"", "\"\"", true},
		{"c:/this/is/harmless", "\"c:/this/is/harmless\"", true},
		{"c:/test&notepad", "\"c:/test&notepad\"", true},
		{"c:/test\"&\"notepad", "", false},
	} {
		got, err := quoteOrEscapeShellPath("cmd", test.unescaped)
		if test.ok {
			assert.NoError(t, err)
			assert.Equal(t, test.escaped, got, fmt.Sprintf("Test %d unescaped = %q", i, test.unescaped))
		} else {
			assert.Error(t, err)
		}
	}
}

func TestShellEscapePowerShell(t *testing.T) {
	for i, test := range []struct {
		unescaped, escaped string
	}{
		{"", "''"},
		{"c:/this/is/harmless", "'c:/this/is/harmless'"},
		{"c:/test&notepad", "'c:/test&notepad'"},
		{"c:/test\"&\"notepad", "'c:/test\"&\"notepad'"},
		{"c:/test'&'notepad", "'c:/test''&''notepad'"},
	} {
		got, err := quoteOrEscapeShellPath("powershell", test.unescaped)
		assert.NoError(t, err)
		assert.Equal(t, test.escaped, got, fmt.Sprintf("Test %d unescaped = %q", i, test.unescaped))
	}
}

func TestRemotePathEncodesRemoteNames(t *testing.T) {
	f := &Fs{
		absRoot: "/srv/root",
		opt: Options{
			Enc: encoder.Display | encoder.EncodeColon,
		},
	}

	assert.Equal(t, "/srv/root/dir/file\uFF1Aname", f.remotePath("dir/file:name"))
	assert.Equal(t, "/srv/root", f.remotePath(""))
}

func TestRemoteShellPathEncodesRemoteNames(t *testing.T) {
	f := &Fs{
		absRoot: "/srv/root",
		opt: Options{
			Enc: encoder.Display | encoder.EncodeColon,
		},
	}

	assert.Equal(t, "/srv/root/dir/file\uFF1Aname", f.remoteShellPath("dir/file:name"))
}

func TestRemoteShellPathEncodesPathOverrideNames(t *testing.T) {
	f := &Fs{
		absRoot: "/srv/root",
		opt: Options{
			Enc:          encoder.Display | encoder.EncodeColon,
			PathOverride: "/shell/root",
		},
	}

	assert.Equal(t, "/shell/root/dir/file\uFF1Aname", f.remoteShellPath("dir/file:name"))

	f.opt.PathOverride = "@/volume"
	assert.Equal(t, "/volume/srv/root/dir/file\uFF1Aname", f.remoteShellPath("dir/file:name"))
}

func TestParseHash(t *testing.T) {
	for i, test := range []struct {
		sshOutput, checksum string
	}{
		{"8dbc7733dbd10d2efc5c0a0d8dad90f958581821  RELEASE.md\n", "8dbc7733dbd10d2efc5c0a0d8dad90f958581821"},
		{"03cfd743661f07975fa2f1220c5194cbaff48451  -\n", "03cfd743661f07975fa2f1220c5194cbaff48451"},
	} {
		got := parseHash([]byte(test.sshOutput))
		assert.Equal(t, test.checksum, got, fmt.Sprintf("Test %d sshOutput = %q", i, test.sshOutput))
	}
}

// fakePublicKey is a minimal ssh.PublicKey for tests. Marshal() is the only
// method exercised by the host-key code (FingerprintSHA256 sha256s it).
type fakePublicKey struct {
	keyType string
	data    []byte
}

func (k *fakePublicKey) Type() string                            { return k.keyType }
func (k *fakePublicKey) Marshal() []byte                         { return k.data }
func (k *fakePublicKey) Verify(_ []byte, _ *ssh.Signature) error { return nil }

func TestParseHostKeysField(t *testing.T) {
	keyA := []byte{0x01, 0x02, 0x03}
	keyB := []byte{0x04, 0x05, 0x06}
	encA := base64.StdEncoding.EncodeToString(keyA)
	encB := base64.StdEncoding.EncodeToString(keyB)

	for _, test := range []struct {
		name    string
		input   fs.CommaSepList
		want    map[string][][]byte
		wantErr bool
	}{
		{name: "Empty", input: fs.CommaSepList{}, want: map[string][][]byte{}},
		{name: "EmptyEntrySkipped", input: fs.CommaSepList{""}, want: map[string][][]byte{}},
		{name: "Single", input: fs.CommaSepList{"ssh-ed25519 " + encA}, want: map[string][][]byte{"ssh-ed25519": {keyA}}},
		{
			name:  "MultiAlgo",
			input: fs.CommaSepList{"ssh-ed25519 " + encA, "rsa-sha2-256 " + encB},
			want:  map[string][][]byte{"ssh-ed25519": {keyA}, "rsa-sha2-256": {keyB}},
		},
		{
			name:  "MultiKeyPerAlgo",
			input: fs.CommaSepList{"ssh-ed25519 " + encA, "ssh-ed25519 " + encB},
			want:  map[string][][]byte{"ssh-ed25519": {keyA, keyB}},
		},
		{
			name:  "EntryLevelWhitespaceTrimmed",
			input: fs.CommaSepList{"  ssh-ed25519 " + encA + "  ", " rsa-sha2-256 " + encB + "\t"},
			want:  map[string][][]byte{"ssh-ed25519": {keyA}, "rsa-sha2-256": {keyB}},
		},
		{name: "MalformedTooFewFields", input: fs.CommaSepList{"ssh-ed25519"}, wantErr: true},
		{name: "MalformedTooManyFields", input: fs.CommaSepList{"ssh-ed25519 " + encA + " trailing"}, wantErr: true},
		{name: "MalformedBase64", input: fs.CommaSepList{"ssh-ed25519 not-base64-!!!"}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseHostKeysField(test.input)
			if test.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestFormatHostKeysFieldRoundTrip(t *testing.T) {
	// Pre-sort the inner slices so the round-trip compare succeeds.
	canonical := map[string][][]byte{
		"ssh-ed25519":  {{0x01}, {0x02}, {0x03}},
		"rsa-sha2-256": {{0x10}, {0xff}},
	}
	formatted := formatHostKeysField(canonical)
	// Deterministic: algos sorted, keys within algo sorted by bytes.
	expected := fs.CommaSepList{
		"rsa-sha2-256 EA==",
		"rsa-sha2-256 /w==",
		"ssh-ed25519 AQ==",
		"ssh-ed25519 Ag==",
		"ssh-ed25519 Aw==",
	}
	assert.Equal(t, expected, formatted)
	// The CommaSepList wire form joins entries with commas.
	assert.Equal(t, "rsa-sha2-256 EA==,rsa-sha2-256 /w==,ssh-ed25519 AQ==,ssh-ed25519 Ag==,ssh-ed25519 Aw==", formatted.String())

	parsed, err := parseHostKeysField(formatted)
	require.NoError(t, err)
	assert.Equal(t, canonical, parsed)

	// Format is independent of input ordering.
	scrambled := map[string][][]byte{
		"rsa-sha2-256": {{0xff}, {0x10}},
		"ssh-ed25519":  {{0x03}, {0x01}, {0x02}},
	}
	assert.Equal(t, expected, formatHostKeysField(scrambled))
}

func TestPinnedHostKeyAlgorithms(t *testing.T) {
	for _, test := range []struct {
		name  string
		input map[string][][]byte
		want  []string
	}{
		{name: "Empty", input: map[string][][]byte{}, want: []string{}},
		{
			name:  "NonRSA",
			input: map[string][][]byte{"ssh-ed25519": {{0x01}}, "ecdsa-sha2-nistp256": {{0x02}}},
			want:  []string{"ecdsa-sha2-nistp256", "ssh-ed25519"},
		},
		{
			// "ssh-rsa" is a key format, not just a signature algorithm, so
			// it must expand to the rsa-sha2 signature algorithms too.
			name:  "RSAExpanded",
			input: map[string][][]byte{"ssh-rsa": {{0x01}}},
			want:  []string{"rsa-sha2-256", "rsa-sha2-512", "ssh-rsa"},
		},
		{
			name:  "RSAAndOthers",
			input: map[string][][]byte{"ssh-rsa": {{0x01}}, "ssh-ed25519": {{0x02}}},
			want:  []string{"rsa-sha2-256", "rsa-sha2-512", "ssh-ed25519", "ssh-rsa"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, pinnedHostKeyAlgorithms(test.input))
		})
	}
}

func TestHostKeyCallbackValidateMatch(t *testing.T) {
	keyBytes := []byte("trusted-key-marshalled-bytes")
	f := &Fs{
		hostKeys: map[string][][]byte{"ssh-ed25519": {keyBytes}},
	}
	cb := f.hostKeyCallback()
	err := cb("example.com:22", nil, &fakePublicKey{keyType: "ssh-ed25519", data: keyBytes})
	assert.NoError(t, err)
}

func TestHostKeyCallbackValidateMismatch(t *testing.T) {
	pinned := []byte("pinned-key-bytes")
	offered := []byte("DIFFERENT-key-bytes")
	f := &Fs{
		hostKeys: map[string][][]byte{"ssh-ed25519": {pinned}},
	}
	cb := f.hostKeyCallback()
	err := cb("example.com:22", nil, &fakePublicKey{keyType: "ssh-ed25519", data: offered})
	require.Error(t, err)
	// Mismatch message should include both fingerprints and a how-to hint.
	assert.Contains(t, err.Error(), "host key mismatch")
	assert.Contains(t, err.Error(), "--sftp-pin-host-key")
}

func TestHostKeyCallbackRejectsCertificate(t *testing.T) {
	f := &Fs{
		opt:      Options{PinHostKey: true},
		hostKeys: map[string][][]byte{},
	}
	cb := f.hostKeyCallback()

	// Wrap a real Ed25519 key in an *ssh.Certificate so the type
	// assertion in the callback fires.
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	sshPub, err := ssh.NewPublicKey(pub)
	require.NoError(t, err)
	cert := &ssh.Certificate{Key: sshPub}

	err = cb("example.com:22", nil, cert)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "certificate")
}

func TestHostKeyCallbackPinHostKeyStashes(t *testing.T) {
	mapper := configmap.Simple{}
	f := &Fs{
		opt:      Options{PinHostKey: true},
		hostKeys: map[string][][]byte{},
		m:        mapper,
	}
	keyBytes := []byte("fresh-key-bytes")
	cb := f.hostKeyCallback()
	err := cb("example.com:22", nil, &fakePublicKey{keyType: "ssh-ed25519", data: keyBytes})
	assert.NoError(t, err)
	pk := f.pendingHostKey.Load()
	require.NotNil(t, pk, "PinHostKey should stash the offered key")
	assert.Equal(t, "ssh-ed25519", pk.algo)
	assert.Equal(t, keyBytes, pk.marshalled)
	// Neither durable nor in-memory state is touched in the callback: the
	// commit happens only after authentication succeeds.
	_, ok := mapper["host_keys"]
	assert.False(t, ok, "callback must not persist; that's commitHostKey's job")
	assert.Empty(t, f.hostKeys["ssh-ed25519"], "callback must not extend in-memory trust set pre-auth")
}

func TestHostKeyCallbackRefusesAtCap(t *testing.T) {
	// Populate host_keys at the cap, then attempt to PinHostKey a brand new
	// algorithm. The callback must refuse to prevent unbounded growth.
	f := &Fs{
		opt:      Options{PinHostKey: true},
		hostKeys: map[string][][]byte{},
	}
	for i := 0; i < maxHostKeys; i++ {
		algo := fmt.Sprintf("test-algo-%d", i)
		f.hostKeys[algo] = [][]byte{{byte(i)}}
	}
	cb := f.hostKeyCallback()
	err := cb("example.com:22", nil, &fakePublicKey{keyType: "ssh-ed25519", data: []byte("new-key")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cap")
	// Nothing was stashed for persistence.
	assert.Nil(t, f.pendingHostKey.Load())
}

func TestHostKeyCallbackRefusesWhenNotPinning(t *testing.T) {
	// pin_host_key=false AND no entry pinned for this algo -> reject.
	f := &Fs{
		opt:      Options{PinHostKey: false},
		hostKeys: map[string][][]byte{},
	}
	cb := f.hostKeyCallback()
	err := cb("example.com:22", nil, &fakePublicKey{keyType: "ssh-ed25519", data: []byte("x")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--sftp-pin-host-key")
}

func TestHostKeyCallbackPinHostKeyRace(t *testing.T) {
	mapper := configmap.Simple{}
	f := &Fs{
		opt:      Options{PinHostKey: true},
		hostKeys: map[string][][]byte{},
		m:        mapper,
	}
	keyBytes := []byte("race-key-bytes")
	cb := f.hostKeyCallback()

	var wg sync.WaitGroup
	const N = 16
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			err := cb("example.com:22", nil, &fakePublicKey{keyType: "ssh-ed25519", data: keyBytes})
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	pk := f.pendingHostKey.Load()
	require.NotNil(t, pk)
	assert.Equal(t, keyBytes, pk.marshalled)
	// No mutation of f.hostKeys pre-auth, even under heavy parallel callbacks.
	assert.Empty(t, f.hostKeys["ssh-ed25519"])
}

func TestCommitHostKey(t *testing.T) {
	mapper := configmap.Simple{}
	f := &Fs{
		name:     "myremote",
		hostKeys: map[string][][]byte{},
		m:        mapper,
	}
	key1 := []byte("ed25519-key-bytes")
	key2 := []byte("rsa-key-bytes")

	f.commitHostKey(&pendingKey{algo: "ssh-ed25519", marshalled: key1, fingerprint: "SHA256:fp1", hostname: "h"})
	f.commitHostKey(&pendingKey{algo: "rsa-sha2-256", marshalled: key2, fingerprint: "SHA256:fp2", hostname: "h"})

	stored, ok := mapper["host_keys"]
	require.True(t, ok, "commitHostKey must persist to the configmap")
	expected := "rsa-sha2-256 " + base64.StdEncoding.EncodeToString(key2) + ",ssh-ed25519 " + base64.StdEncoding.EncodeToString(key1)
	assert.Equal(t, expected, stored)

	// commitHostKey must also extend the in-memory trust set so the validate
	// path on subsequent connections recognises the freshly-pinned keys.
	assert.Equal(t, [][]byte{key1}, f.hostKeys["ssh-ed25519"])
	assert.Equal(t, [][]byte{key2}, f.hostKeys["rsa-sha2-256"])

	// Second commit of the same key is a no-op (dedupe).
	f.commitHostKey(&pendingKey{algo: "ssh-ed25519", marshalled: key1, fingerprint: "SHA256:fp1", hostname: "h"})
	assert.Equal(t, expected, mapper["host_keys"])
	assert.Equal(t, [][]byte{key1}, f.hostKeys["ssh-ed25519"])
}

func TestCommitHostKeyRefusesToOverwriteMalformed(t *testing.T) {
	// host_keys parsed cleanly at NewFs time, then was hand-edited to
	// something garbage before commit ran. We must not overwrite that
	// value with just our new entry (which would destroy any other valid
	// entries it once contained).
	const garbage = "this is not a valid host_keys value"
	mapper := configmap.Simple{"host_keys": garbage}
	f := &Fs{
		name:     "myremote",
		hostKeys: map[string][][]byte{},
		m:        mapper,
	}
	key := []byte("would-have-been-pinned")
	f.commitHostKey(&pendingKey{algo: "ssh-ed25519", marshalled: key, fingerprint: "SHA256:fp", hostname: "h"})

	// Durable storage is preserved as-is.
	assert.Equal(t, garbage, mapper["host_keys"])
	// In-memory trust set is NOT extended — we refuse to pretend we pinned.
	assert.Empty(t, f.hostKeys["ssh-ed25519"])
}

func TestParseUsage(t *testing.T) {
	for i, test := range []struct {
		sshOutput string
		usage     [3]int64
	}{
		{"Filesystem     1K-blocks     Used Available Use% Mounted on\n/dev/root       91283092 81111888  10154820  89% /", [3]int64{93473886208, 83058573312, 10398535680}},
		{"Filesystem     1K-blocks  Used Available Use% Mounted on\ntmpfs             818256  1636    816620   1% /run", [3]int64{837894144, 1675264, 836218880}},
		{"Filesystem   1024-blocks     Used Available Capacity iused      ifree %iused  Mounted on\n/dev/disk0s2   244277768 94454848 149566920    39%  997820 4293969459    0%   /", [3]int64{250140434432, 96721764352, 153156526080}},
	} {
		gotSpaceTotal, gotSpaceUsed, gotSpaceAvail := parseUsage([]byte(test.sshOutput))
		assert.Equal(t, test.usage, [3]int64{gotSpaceTotal, gotSpaceUsed, gotSpaceAvail}, fmt.Sprintf("Test %d sshOutput = %q", i, test.sshOutput))
	}
}

// internalTestHostKeyPinning exercises the full PinHostKey host-key flow against the
// live SFTP server by mirroring the running Fs's connection details into a
// temporary rclone config under a fresh remote name, then driving that remote
// through pin/validate/mismatch.
func (f *Fs) internalTestHostKeyPinning(t *testing.T) {
	ctx := context.Background()

	if len(f.opt.SSH) > 0 {
		t.Skip("host key pinning is bypassed when the ssh option is set")
	}

	// Mirror config from the running Fs into a fresh `pinhostkey` remote
	var b strings.Builder
	b.WriteString("[pinhostkey]\n")
	b.WriteString("type = sftp\n")
	for _, opt := range fs.MustFind("sftp").Options {
		// known_hosts_file must not be mirrored as it takes precedence
		// over pin_host_key and would disable the pinning under test.
		if opt.Name == "pin_host_key" || opt.Name == "host_keys" || opt.Name == "known_hosts_file" {
			continue
		}
		v, ok := f.m.Get(opt.Name)
		if !ok || v == "" {
			continue
		}
		fmt.Fprintf(&b, "%s = %s\n", opt.Name, v)
	}
	b.WriteString("pin_host_key = true\n")

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "rclone.conf")
	require.NoError(t, os.WriteFile(configPath, []byte(b.String()), 0600))

	oldPath := config.GetConfigPath()
	require.NoError(t, config.SetConfigPath(configPath))
	t.Cleanup(func() {
		cache.ClearConfig("pinhostkey")
		_ = config.SetConfigPath(oldPath)
	})

	t.Run("FirstConnectPins", func(t *testing.T) {
		cache.ClearConfig("pinhostkey")
		_, err := fs.NewFs(ctx, "pinhostkey:")
		if err != nil && strings.Contains(err.Error(), "SSH certificate") {
			t.Skipf("server presents an SSH certificate; pin_host_key cannot validate: %v", err)
		}
		require.NoError(t, err)
		raw, err := os.ReadFile(configPath)
		require.NoError(t, err)
		require.Regexp(t, `(?m)^host_keys\s*=\s*\S+\s+\S+`, string(raw),
			"PinHostKey should have written host_keys into the temp config")
	})

	t.Run("SecondConnectValidates", func(t *testing.T) {
		before, err := os.ReadFile(configPath)
		require.NoError(t, err)
		cache.ClearConfig("pinhostkey")
		_, err = fs.NewFs(ctx, "pinhostkey:")
		require.NoError(t, err)
		after, err := os.ReadFile(configPath)
		require.NoError(t, err)
		assert.Equal(t, string(before), string(after),
			"validate path must not rewrite host_keys")
	})

	t.Run("MismatchRejected", func(t *testing.T) {
		// Substitute a different valid ed25519 key so the mismatch (not the
		// malformed-config) error path fires.
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		sshPub, err := ssh.NewPublicKey(priv.Public())
		require.NoError(t, err)
		fake := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(sshPub.Marshal())
		require.NoError(t, config.SetValueAndSave("pinhostkey", "host_keys", fake))

		cache.ClearConfig("pinhostkey")
		_, err = fs.NewFs(ctx, "pinhostkey:")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "host key mismatch")
	})
}

// InternalTest dispatches integration-only tests that need a live SFTP
// connection. fstests.Run invokes this after the standard test suite.
func (f *Fs) InternalTest(t *testing.T) {
	t.Run("HostKeyPinning", f.internalTestHostKeyPinning)
}

// Check interface
var _ fstests.InternalTester = (*Fs)(nil)

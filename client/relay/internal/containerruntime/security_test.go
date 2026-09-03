package containerruntime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/runtimemanifest"
)

func TestUnixSecretServerDeliversOneFramedEnvelopeAndRemovesSocket(t *testing.T) {
	directory := shortSocketTestDirectory(t)
	server, err := newUnixSecretServer(directory, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	secrets := testSecrets()
	lease, err := server.Open(context.Background(), secrets)
	if err != nil {
		t.Fatal(err)
	}
	path := lease.Path()
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	payload := readRawFrame(t, connection)
	if second, err := net.DialTimeout("unix", path, 50*time.Millisecond); err == nil {
		_ = second.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		probe := make([]byte, 1)
		if count, _ := second.Read(probe); count != 0 {
			t.Fatal("second client received bootstrap secret data")
		}
		_ = second.Close()
	}
	var envelope struct {
		Schema     int    `json:"schema"`
		APIToken   string `json:"api_auth_token"`
		AdminToken string `json:"admin_auth_token"`
	}
	if err := decodeStrict(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Schema != 1 || envelope.APIToken != string(secrets.APIToken) || envelope.AdminToken != string(secrets.AdminToken) {
		t.Fatalf("unexpected envelope: %#v", envelope)
	}
	writeRawFrame(t, connection, []byte(`{"schema":1,"accepted":true}`))
	if err := connection.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bootstrap socket remains after ACK: %v", err)
	}
}

func TestUnixSecretServerRejectsTrailingACKDataAndEarlyDisconnect(t *testing.T) {
	for _, test := range []struct {
		name string
		send func(*testing.T, *net.UnixConn)
	}{
		{name: "trailing data", send: func(t *testing.T, connection *net.UnixConn) {
			_ = readRawFrame(t, connection)
			ack := framed([]byte(`{"schema":1,"accepted":true}`))
			ack = append(ack, 'x')
			if err := writeAll(connection, ack); err != nil {
				t.Fatal(err)
			}
			_ = connection.CloseWrite()
		}},
		{name: "early disconnect", send: func(_ *testing.T, connection *net.UnixConn) {
			_ = connection.Close()
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, err := newUnixSecretServer(shortSocketTestDirectory(t), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			lease, err := server.Open(context.Background(), testSecrets())
			if err != nil {
				t.Fatal(err)
			}
			connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: lease.Path(), Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			test.send(t, connection)
			if err := lease.Wait(context.Background()); !errors.Is(err, ErrCredential) {
				t.Fatalf("Wait error = %v", err)
			}
			_ = connection.Close()
		})
	}
}

func TestUnixSecretServerTimesOutWithoutClient(t *testing.T) {
	server, err := newUnixSecretServer(shortSocketTestDirectory(t), 25*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := server.Open(context.Background(), testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Wait(context.Background()); !errors.Is(err, ErrCredential) {
		t.Fatalf("Wait error = %v", err)
	}
	if _, err := os.Lstat(lease.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("timed-out socket remains: %v", err)
	}
}

func TestSystemKeychainCreatesSecretsThroughStdinOnly(t *testing.T) {
	runner := &keychainRecordingRunner{values: map[string][]byte{}}
	keychain := newSystemKeychainWithRunner(runner)
	secrets, err := keychain.Ensure(context.Background(), "test-user")
	if err != nil {
		t.Fatal(err)
	}
	defer zeroSecrets(&secrets)
	if !validSecret(secrets.APIToken) || !validSecret(secrets.AdminToken) || bytes.Equal(secrets.APIToken, secrets.AdminToken) {
		t.Fatal("generated Keychain tokens are not distinct 32-byte base64url values")
	}
	if len(runner.addInputs) != 2 {
		t.Fatalf("add input count = %d", len(runner.addInputs))
	}
	for _, call := range runner.calls {
		joined := strings.Join(call.arguments, "\x00")
		if strings.Contains(joined, string(secrets.APIToken)) || strings.Contains(joined, string(secrets.AdminToken)) {
			t.Fatalf("secret appeared in argv: %q", call.arguments)
		}
		if len(call.arguments) > 0 && call.arguments[0] == "add-generic-password" {
			if call.arguments[len(call.arguments)-1] != "-w" {
				t.Fatalf("Keychain password prompt flag is not last: %q", call.arguments)
			}
		}
	}
}

func TestValidSecretRejectsNonCanonicalBase64URLAlias(t *testing.T) {
	canonical := append([]byte(nil), testSecrets().APIToken...)
	alias := append([]byte(nil), canonical...)
	alias[len(alias)-1] = 'F' // Same high data bits as canonical trailing 'E'.
	canonicalDecoded, canonicalErr := base64.RawURLEncoding.DecodeString(string(canonical))
	aliasDecoded, aliasErr := base64.RawURLEncoding.DecodeString(string(alias))
	if canonicalErr != nil || aliasErr != nil || !bytes.Equal(canonicalDecoded, aliasDecoded) {
		t.Fatalf("test value is not a base64url tail-bit alias: canonical=%v alias=%v", canonicalErr, aliasErr)
	}
	if !validSecret(canonical) || validSecret(alias) {
		t.Fatal("secret canonicalization contract was not enforced")
	}
}

func TestDefaultBootstrapSocketDirectoryIsFixedAndShort(t *testing.T) {
	directory := DefaultBootstrapSocketDirectory()
	if !strings.HasPrefix(directory, "/private/tmp/opencodex-relay-runtime-") || !filepath.IsAbs(directory) {
		t.Fatalf("directory = %q", directory)
	}
	path := filepath.Join(directory, "b-"+strings.Repeat("0", 32))
	if len(path) > 103 {
		t.Fatalf("Darwin UDS path is too long: %d %q", len(path), path)
	}
}

func TestAppleInstallIdentityParsersRequireExactIdentity(t *testing.T) {
	official := []byte("Executable=/usr/local/bin/container\nIdentifier=com.apple.container.cli\nTeamIdentifier=UPBK2H6LZM\n")
	if !hasExactCodesignIdentity(official, appleCLIIdentifier, appleCLITeamIdentifier) {
		t.Fatal("official identifier and team were rejected")
	}
	for _, value := range []string{
		"Identifier=com.apple.container.cli.evil\n",
		"Identifier=com.apple.container.cli\nIdentifier=com.apple.container.cli\n",
		"prefix Identifier=com.apple.container.cli\n",
		"Identifier=com.apple.container.cli\nTeamIdentifier=OTHERTEAM1\n",
		"Identifier=com.apple.container.cli\nTeamIdentifier=UPBK2H6LZM\nTeamIdentifier=UPBK2H6LZM\n",
	} {
		if hasExactCodesignIdentity([]byte(value), appleCLIIdentifier, appleCLITeamIdentifier) {
			t.Fatalf("non-exact identity was accepted: %q", value)
		}
	}
	receipt := []byte(`<?xml version="1.0"?><plist version="1.0"><dict><key>pkgid</key><string>com.apple.container-installer</string><key>pkg-version</key><string>1.3.1</string></dict></plist>`)
	identifier, version, ok := parsePackageReceipt(receipt)
	if !ok || identifier != applePackageReceipt || version != "1.3.1" {
		t.Fatalf("receipt = %q %q %t", identifier, version, ok)
	}
	duplicate := []byte(`<?xml version="1.0"?><plist><dict><key>pkgid</key><string>com.apple.container-installer</string><key>pkgid</key><string>evil</string><key>pkg-version</key><string>1.3.1</string></dict></plist>`)
	if _, _, ok := parsePackageReceipt(duplicate); ok {
		t.Fatal("duplicate receipt key was accepted")
	}
	if !receiptOwnsCLI([]byte("./bin/container\nshare/doc\n")) || receiptOwnsCLI([]byte("bin/container-helper\n")) {
		t.Fatal("receipt file ownership was not matched exactly")
	}
}

func TestProtectedExecutableRejectsWritableSymlinkAndWrongOwnerComponents(t *testing.T) {
	trustedRoot := t.TempDir()
	parent := filepath.Join(trustedRoot, "usr", "local", "bin")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(parent, "container")
	if err := os.WriteFile(executable, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !protectedExecutable(executable, trustedRoot, os.Geteuid()) {
		t.Fatal("protected executable was rejected")
	}
	if runtime.GOOS == "darwin" {
		output, err := systemCommandRunner{}.Run(context.Background(), "/bin/chmod", []string{
			"+a", "everyone allow add_file,delete_child", parent,
		}, nil, 4<<10)
		zeroCommandOutput(&output)
		if err != nil {
			t.Fatalf("create extended ACL fixture: %v", err)
		}
		if protectedExecutable(executable, trustedRoot, os.Geteuid()) {
			t.Fatal("extended ACL on parent was accepted")
		}
		output, err = systemCommandRunner{}.Run(context.Background(), "/bin/chmod", []string{"-N", parent}, nil, 4<<10)
		zeroCommandOutput(&output)
		if err != nil {
			t.Fatalf("remove extended ACL fixture: %v", err)
		}
	}
	if err := os.Chmod(parent, 0o720); err != nil {
		t.Fatal(err)
	}
	if protectedExecutable(executable, trustedRoot, os.Geteuid()) {
		t.Fatal("group-writable parent was accepted")
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if protectedExecutable(executable, trustedRoot, os.Geteuid()+1) {
		t.Fatal("wrong-owner path was accepted")
	}
	if err := os.Remove(executable); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(trustedRoot, "replacement"), executable); err != nil {
		t.Fatal(err)
	}
	if protectedExecutable(executable, trustedRoot, os.Geteuid()) {
		t.Fatal("symlink executable was accepted")
	}
}

func TestRemoveGenerationRejectsSymlinkedHomesParentWithoutDeletingTarget(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtime")
	store, err := newStateStore(root)
	if err != nil {
		t.Fatalf("prepare runtime store: %v", err)
	}
	if err := store.prepareRoot(); err != nil {
		t.Fatalf("prepare runtime root: %v", err)
	}
	external := t.TempDir()
	externalGeneration := filepath.Join(external, "generation-0001")
	if err := os.Mkdir(externalGeneration, 0o700); err != nil {
		t.Fatal(err)
	}
	witness := filepath.Join(externalGeneration, "must-survive")
	if err := os.WriteFile(witness, []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(root, "homes")); err != nil {
		t.Fatal(err)
	}
	if err := store.removeGeneration(1); !errors.Is(err, ErrUnsafeState) {
		t.Fatalf("symlinked homes removal error = %v", err)
	}
	if data, err := os.ReadFile(witness); err != nil || string(data) != "external" {
		t.Fatalf("external generation was changed: data=%q err=%v", data, err)
	}
}

func TestExtractSystemVersionsRequiresOneNamedCLI(t *testing.T) {
	data := []byte(`[{"version":"1.3.1","buildType":"release","commit":"a","appName":"container"},{"version":"1.3.2","buildType":"release","commit":"b","appName":"container-apiserver"}]`)
	cli, versions, ok := extractSystemVersions(data)
	if !ok || cli != "1.3.1" || !reflect.DeepEqual(versions, []string{"1.3.1", "1.3.2"}) {
		t.Fatalf("versions = %q %#v %t", cli, versions, ok)
	}
	if _, _, ok := extractSystemVersions([]byte(`[{"version":"1.3.1","appName":"container"},{"version":"1.3.1","appName":"container"}]`)); ok {
		t.Fatal("duplicate CLI rows were accepted")
	}
}

func TestExtractServiceStateRequiresExactOfficialShape(t *testing.T) {
	valid := []byte(`{"status":"running","appRoot":"/a","installRoot":"/i","apiServerVersion":"1.3.1","apiServerCommit":"abc","apiServerBuild":"release","apiServerAppName":"container-apiserver","logRoot":null}`)
	if state := extractServiceState(valid); state != "running" {
		t.Fatalf("valid service state = %q", state)
	}
	for name, value := range map[string][]byte{
		"nested decoy": []byte(`{"outer":{"status":"running"}}`),
		"alias":        []byte(`{"status":"ready","appRoot":"/a","installRoot":"/i","apiServerVersion":"1.3.1","apiServerCommit":"abc","apiServerBuild":"release","apiServerAppName":"container-apiserver"}`),
		"unknown":      []byte(`{"status":"running","appRoot":"/a","installRoot":"/i","apiServerVersion":"1.3.1","apiServerCommit":"abc","apiServerBuild":"release","apiServerAppName":"container-apiserver","extra":true}`),
		"incomplete":   []byte(`{"status":"running","appRoot":"/a","installRoot":"/i","apiServerVersion":"1.3.1","apiServerCommit":"abc","apiServerBuild":"release","apiServerAppName":""}`),
	} {
		t.Run(name, func(t *testing.T) {
			if state := extractServiceState(value); state != "unknown" && name != "alias" {
				t.Fatalf("invalid service state = %q", state)
			} else if name == "alias" && state != "stopped" {
				t.Fatalf("non-running official state = %q", state)
			}
		})
	}
}

func TestAppleCLIStartUsesExactHardenedArguments(t *testing.T) {
	directory := shortSocketTestDirectory(t)
	spec, listener := liveStartSpec(t, directory)
	defer listener.Close()
	runner := &scriptedCommandRunner{run: func(_ string, arguments []string, _ []byte) (commandOutput, error) {
		if len(arguments) > 0 && arguments[0] == "list" {
			return commandOutput{stdout: []byte("[]")}, nil
		}
		return commandOutput{}, nil
	}}
	cli := newAppleCLIWithRunner(runner)
	cli.probePort = func() error { return nil }
	cli.socketDirectory = directory
	containerID, err := cli.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if containerID != ContainerName || len(runner.calls) != 2 {
		t.Fatalf("container = %q, calls = %#v", containerID, runner.calls)
	}
	expected := []string{
		"run", "--detach", "--name", ContainerName, "--platform", "linux/arm64",
		"--uid", osUID(), "--gid", osGID(), "--read-only", "--cap-drop", "ALL", "--init",
		"--cpus", "2", "--memory", "1G", "--tmpfs", "/tmp", "--publish", "127.0.0.1:10210:10100/tcp",
	}
	labels := ownedLabels(spec)
	for _, key := range []string{labelOwner, labelInstallation, labelOperation, labelManifest, labelIndexDigest, labelGeneration} {
		expected = append(expected, "--label", key+"="+labels[key])
	}
	expected = append(expected,
		"--mount", "type=bind,source="+spec.StatePath+",target="+GuestStatePath,
		"--mount", "type=bind,source="+spec.SocketPath+",target="+GuestBootstrapSocket,
		spec.ImageReference,
	)
	if !reflect.DeepEqual(runner.calls[1].arguments, expected) {
		t.Fatalf("run arguments mismatch\n got: %#v\nwant: %#v", runner.calls[1].arguments, expected)
	}
	for _, argument := range runner.calls[1].arguments {
		if strings.HasPrefix(argument, "--env") || strings.Contains(argument, "OPENCODEX_") {
			t.Fatalf("environment secret surface in argv: %q", argument)
		}
	}
}

func TestAppleCLICanaryNetworkIsCompileTimeOnlyAndReadBackExactly(t *testing.T) {
	directory := shortSocketTestDirectory(t)
	spec, listener := liveStartSpec(t, directory)
	defer listener.Close()
	network := "ocx-lifecycle-canary-012345abcdef"
	runner := &scriptedCommandRunner{run: func(_ string, arguments []string, _ []byte) (commandOutput, error) {
		if len(arguments) > 0 && arguments[0] == "list" {
			return commandOutput{stdout: []byte("[]")}, nil
		}
		return commandOutput{}, nil
	}}
	cli := newAppleCLIWithRunner(runner)
	cli.probePort = func() error { return nil }
	cli.socketDirectory = directory
	cli.networkName = network
	if _, err := cli.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	arguments := runner.calls[1].arguments
	if value := argumentValue(arguments, "--network"); value != network {
		t.Fatalf("canary network argument = %q", value)
	}

	object := inspectedContainer(spec)
	object["status"] = map[string]any{
		"state":    "running",
		"networks": []any{map[string]any{"network": network}},
	}
	body, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	runner = &scriptedCommandRunner{run: func(_ string, _ []string, _ []byte) (commandOutput, error) {
		return commandOutput{stdout: append([]byte(nil), body...)}, nil
	}}
	cli = newAppleCLIWithRunner(runner)
	cli.socketDirectory = directory
	cli.networkName = network
	if err := cli.VerifyContainer(context.Background(), ContainerName, spec); err != nil {
		t.Fatalf("exact canary network was rejected: %v", err)
	}

	for name, networks := range map[string][]any{
		"default plus canary": {map[string]any{"network": "default"}, map[string]any{"network": network}},
		"wrong network":       {map[string]any{"network": "default"}},
		"missing network":     {},
	} {
		t.Run(name, func(t *testing.T) {
			object["status"] = map[string]any{"state": "running", "networks": networks}
			body, _ = json.Marshal(object)
			if err := cli.VerifyContainer(context.Background(), ContainerName, spec); !errors.Is(err, ErrUnsafeState) {
				t.Fatalf("network drift error = %v", err)
			}
		})
	}

	cli.networkName = "user-selected-network"
	if _, err := cli.Start(context.Background(), spec); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("runtime-configurable network error = %v", err)
	}
}

func TestAppleCLIImageInspectUsesSupportedExactArguments(t *testing.T) {
	fixture := newManagerFixture(t)
	candidate := fixture.candidate(t, "2.40.0", 1, 1)
	record, err := recordFromCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	spec := startSpec(strings.Repeat("1", 64), strings.Repeat("2", 64), record, 1, "/private/tmp/homes/generation-0001", "")
	object := inspectedRuntimeImage(spec, candidate.Manifest)
	body, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	runner := &scriptedCommandRunner{run: func(_ string, arguments []string, _ []byte) (commandOutput, error) {
		if !reflect.DeepEqual(arguments, []string{"image", "inspect", spec.ImageReference}) {
			t.Fatalf("image inspect arguments = %#v", arguments)
		}
		return commandOutput{stdout: body}, nil
	}}
	if err := newAppleCLIWithRunner(runner).VerifyImage(context.Background(), spec, candidate.Manifest); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name   string
		mutate func([]any)
	}{
		{name: "wrong index descriptor", mutate: func(resources []any) {
			resource := resources[0].(map[string]any)
			resource["configuration"].(map[string]any)["descriptor"].(map[string]any)["digest"] = "sha256:" + strings.Repeat("9", 64)
		}},
		{name: "unrelated nested spoof evidence", mutate: func(resources []any) {
			resource := resources[0].(map[string]any)
			resource["variants"].([]any)[0].(map[string]any)["digest"] = "sha256:" + strings.Repeat("9", 64)
			spoof := expectedRuntimeImageLabels(candidate.Manifest)
			spoof["digest"] = spec.ARM64Digest
			resource["spoof"] = spoof
		}},
		{name: "digest and labels split across variants", mutate: func(resources []any) {
			resource := resources[0].(map[string]any)
			variants := resource["variants"].([]any)
			armConfig := variants[0].(map[string]any)["config"].(map[string]any)["config"].(map[string]any)
			delete(armConfig, "Labels")
			resource["variants"] = append(variants, inspectedRuntimeImageVariant("linux", "amd64", "sha256:"+strings.Repeat("8", 64), expectedRuntimeImageLabels(candidate.Manifest)))
		}},
		{name: "duplicate linux arm64 variant", mutate: func(resources []any) {
			resource := resources[0].(map[string]any)
			variants := resource["variants"].([]any)
			resource["variants"] = append(variants, inspectedRuntimeImageVariant("linux", "arm64", spec.ARM64Digest, expectedRuntimeImageLabels(candidate.Manifest)))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			resources := inspectedRuntimeImage(spec, candidate.Manifest)
			test.mutate(resources)
			invalid, err := json.Marshal(resources)
			if err != nil {
				t.Fatal(err)
			}
			runner := &scriptedCommandRunner{run: func(_ string, _ []string, _ []byte) (commandOutput, error) {
				return commandOutput{stdout: invalid}, nil
			}}
			if err := newAppleCLIWithRunner(runner).VerifyImage(context.Background(), spec, candidate.Manifest); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("structurally unbound image error=%v", err)
			}
		})
	}
}

func inspectedRuntimeImage(spec StartSpec, manifest runtimemanifest.Manifest) []any {
	return []any{map[string]any{
		"id": strings.TrimPrefix(spec.IndexDigest, "sha256:"),
		"configuration": map[string]any{
			"name":       spec.ImageReference,
			"descriptor": map[string]any{"digest": spec.IndexDigest},
		},
		"variants": []any{
			inspectedRuntimeImageVariant("linux", "arm64", spec.ARM64Digest, expectedRuntimeImageLabels(manifest)),
		},
	}}
}

func inspectedRuntimeImageVariant(osName, architecture, digest string, labels map[string]any) map[string]any {
	return map[string]any{
		"platform": map[string]any{"os": osName, "architecture": architecture},
		"digest":   digest,
		"size":     1,
		"config": map[string]any{
			"os": osName, "architecture": architecture,
			"config": map[string]any{"Labels": labels},
			"rootfs": map[string]any{"type": "layers", "diff_ids": []any{}},
		},
	}
}

func expectedRuntimeImageLabels(manifest runtimemanifest.Manifest) map[string]any {
	return map[string]any{
		"org.opencontainers.image.source":                  imageSourceURL,
		"org.opencontainers.image.version":                 manifest.ArtifactVersion,
		"io.github.novelkr.opencodex.upstream.version":     manifest.Upstream.Version,
		"io.github.novelkr.opencodex.upstream.revision":    manifest.Upstream.Revision,
		"io.github.novelkr.opencodex.public-core.revision": manifest.Source.Revision,
	}
}

func TestAppleCLIVerifyContainerAllowsRemovedOneShotSocketAndRejectsForeignOwner(t *testing.T) {
	directory := shortSocketTestDirectory(t)
	spec := readbackStartSpec(t, directory)
	object := inspectedContainer(spec)
	body, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	runner := &scriptedCommandRunner{run: func(_ string, _ []string, _ []byte) (commandOutput, error) {
		return commandOutput{stdout: append([]byte(nil), body...)}, nil
	}}
	cli := newAppleCLIWithRunner(runner)
	cli.socketDirectory = directory
	if err := cli.VerifyContainer(context.Background(), ContainerName, spec); err != nil {
		t.Fatalf("removed one-shot socket should be valid read-back: %v", err)
	}
	if len(runner.calls) != 1 || !reflect.DeepEqual(runner.calls[0].arguments, []string{"inspect", ContainerName}) {
		t.Fatalf("container inspect arguments = %#v", runner.calls)
	}
	configuration := object["configuration"].(map[string]any)
	mounts := configuration["mounts"].([]map[string]any)
	configuration["mounts"] = mounts[:2]
	body, _ = json.Marshal(object)
	if err := cli.VerifyContainer(context.Background(), ContainerName, spec); !errors.Is(err, ErrUnsafeState) {
		t.Fatalf("missing /tmp tmpfs error = %v", err)
	}
	configuration["mounts"] = append(append([]map[string]any(nil), mounts...), map[string]any{"type": "tmpfs", "target": "/tmp"})
	body, _ = json.Marshal(object)
	if err := cli.VerifyContainer(context.Background(), ContainerName, spec); !errors.Is(err, ErrUnsafeState) {
		t.Fatalf("duplicate /tmp tmpfs error = %v", err)
	}
	configuration["mounts"] = append(mounts, map[string]any{"type": "bind", "source": "/Users", "target": "/host"})
	body, _ = json.Marshal(object)
	if err := cli.VerifyContainer(context.Background(), ContainerName, spec); !errors.Is(err, ErrUnsafeState) {
		t.Fatalf("unexpected host mount error = %v", err)
	}
	configuration["mounts"] = []map[string]any{
		mounts[0], mounts[1], {"type": "bind", "source": "tmpfs", "target": "/tmp"},
	}
	body, _ = json.Marshal(object)
	if err := cli.VerifyContainer(context.Background(), ContainerName, spec); !errors.Is(err, ErrUnsafeState) {
		t.Fatalf("conflicting /tmp mount type error = %v", err)
	}
	configuration["mounts"] = []map[string]any{
		mounts[0], mounts[1], {"type": "tmpfs", "source": "/foreign", "target": "/tmp"},
	}
	body, _ = json.Marshal(object)
	if err := cli.VerifyContainer(context.Background(), ContainerName, spec); !errors.Is(err, ErrUnsafeState) {
		t.Fatalf("conflicting /tmp mount source error = %v", err)
	}
	configuration["mounts"] = mounts
	publishedPort := configuration["publishedPorts"].([]map[string]any)[0]
	delete(publishedPort, "count")
	body, _ = json.Marshal(object)
	if err := cli.VerifyContainer(context.Background(), ContainerName, spec); !errors.Is(err, ErrUnsafeState) {
		t.Fatalf("missing published port count error = %v", err)
	}
	for _, count := range []int{0, 2} {
		publishedPort["count"] = count
		body, _ = json.Marshal(object)
		if err := cli.VerifyContainer(context.Background(), ContainerName, spec); !errors.Is(err, ErrUnsafeState) {
			t.Fatalf("published port count %d error = %v", count, err)
		}
	}
	publishedPort["count"] = 1
	publishedPort["hostAddress"] = map[string]any{"address": "0.0.0.0", "decoy": "127.0.0.1"}
	body, _ = json.Marshal(object)
	if err := cli.VerifyContainer(context.Background(), ContainerName, spec); !errors.Is(err, ErrUnsafeState) {
		t.Fatalf("structured host address decoy error = %v", err)
	}
	publishedPort["hostAddress"] = "127.0.0.1"
	configuration["resources"].(map[string]any)["cpus"] = 4
	body, _ = json.Marshal(object)
	if err := cli.VerifyContainer(context.Background(), ContainerName, spec); !errors.Is(err, ErrUnsafeState) {
		t.Fatalf("resource drift error = %v", err)
	}
	configuration["resources"].(map[string]any)["cpus"] = 2
	configuration["initProcess"].(map[string]any)["user"].(map[string]any)["id"].(map[string]any)["uid"] = os.Geteuid() + 1
	body, _ = json.Marshal(object)
	if err := cli.VerifyContainer(context.Background(), ContainerName, spec); !errors.Is(err, ErrUnsafeState) {
		t.Fatalf("user drift error = %v", err)
	}
	configuration["initProcess"].(map[string]any)["user"].(map[string]any)["id"].(map[string]any)["uid"] = os.Geteuid()
	configuration["image"].(map[string]any)["reference"] = runtimemanifest.ProductionImageRepository + ":latest"
	body, _ = json.Marshal(object)
	if err := cli.VerifyContainer(context.Background(), ContainerName, spec); !errors.Is(err, ErrUnsafeState) {
		t.Fatalf("image drift error = %v", err)
	}
	configuration["image"].(map[string]any)["reference"] = spec.ImageReference
	configuration["labels"].(map[string]string)[labelInstallation] = strings.Repeat("9", 64)
	body, _ = json.Marshal(object)
	if err := cli.VerifyContainer(context.Background(), ContainerName, spec); !errors.Is(err, ErrForeignResource) {
		t.Fatalf("foreign owner error = %v", err)
	}
}

func TestAppleCLIStopAndDeleteRequireCompleteStartWitness(t *testing.T) {
	directory := shortSocketTestDirectory(t)
	spec := readbackStartSpec(t, directory)
	object := inspectedContainer(spec)
	body, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	runner := &scriptedCommandRunner{run: func(_ string, arguments []string, _ []byte) (commandOutput, error) {
		if len(arguments) > 0 && arguments[0] == "inspect" {
			return commandOutput{stdout: append([]byte(nil), body...)}, nil
		}
		return commandOutput{}, nil
	}}
	cli := newAppleCLIWithRunner(runner)
	cli.socketDirectory = directory
	if err := cli.Stop(context.Background(), ContainerName, spec); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || !reflect.DeepEqual(runner.calls[0].arguments, []string{"inspect", ContainerName}) ||
		!reflect.DeepEqual(runner.calls[1].arguments, []string{"stop", "--time", "15", ContainerName}) {
		t.Fatalf("stop calls = %#v", runner.calls)
	}

	// Matching the public owner and installation labels alone is insufficient:
	// the operation witness distinguishes the exact container this transaction
	// is authorized to mutate.
	configuration := object["configuration"].(map[string]any)
	configuration["labels"].(map[string]string)[labelOperation] = strings.Repeat("9", 64)
	body, err = json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	runner.calls = nil
	runner.mu.Unlock()
	if err := cli.Delete(context.Background(), ContainerName, spec); !errors.Is(err, ErrForeignResource) {
		t.Fatalf("drifted operation ownership error = %v", err)
	}
	if len(runner.calls) != 1 || !reflect.DeepEqual(runner.calls[0].arguments, []string{"inspect", ContainerName}) {
		t.Fatalf("foreign container reached delete mutation: %#v", runner.calls)
	}
}

func TestAppleCLIContainerStateRequiresExactRunningOwnedInspect(t *testing.T) {
	directory := shortSocketTestDirectory(t)
	spec := readbackStartSpec(t, directory)
	object := inspectedContainer(spec)
	body, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	runner := &scriptedCommandRunner{run: func(_ string, arguments []string, _ []byte) (commandOutput, error) {
		if !reflect.DeepEqual(arguments, []string{"inspect", ContainerName}) {
			t.Fatalf("container state arguments = %#v", arguments)
		}
		return commandOutput{stdout: append([]byte(nil), body...)}, nil
	}}
	cli := newAppleCLIWithRunner(runner)
	cli.socketDirectory = directory

	for _, test := range []struct {
		name  string
		state any
		want  FixedContainerState
	}{
		{name: "running", state: "running", want: FixedContainerRunningOwned},
		{name: "stopped", state: "stopped", want: FixedContainerStoppedOwned},
		{name: "stopping", state: "stopping", want: FixedContainerUnknown},
		{name: "exited", state: "exited", want: FixedContainerUnknown},
		{name: "unknown spelling", state: "ready", want: FixedContainerUnknown},
		{name: "missing", state: nil, want: FixedContainerUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			status := map[string]any{}
			if test.state != nil {
				status["state"] = test.state
			}
			object["status"] = status
			body, _ = json.Marshal(object)
			got, err := cli.ContainerState(context.Background(), ContainerName, spec)
			if err != nil || got != test.want {
				t.Fatalf("ContainerState = %q, %v; want %q", got, err, test.want)
			}
		})
	}

	object["status"] = map[string]any{"state": "running"}
	configuration := object["configuration"].(map[string]any)
	configuration["labels"].(map[string]string)[labelOperation] = strings.Repeat("9", 64)
	body, _ = json.Marshal(object)
	if got, err := cli.ContainerState(context.Background(), ContainerName, spec); err != nil || got != FixedContainerForeign {
		t.Fatalf("foreign ContainerState = %q, %v", got, err)
	}

	body = []byte("[]")
	if got, err := cli.ContainerState(context.Background(), ContainerName, spec); err != nil || got != FixedContainerAbsent {
		t.Fatalf("absent ContainerState = %q, %v", got, err)
	}
}

func TestAppleCLIRefusesForeignFixedContainerAndOccupiedPort(t *testing.T) {
	foreignList := []byte(`[{"id":"opencodex-relay-runtime","configuration":{"id":"opencodex-relay-runtime","labels":{"io.github.novelkr.opencodex.runtime.owner":"other","io.github.novelkr.opencodex.runtime.installation":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}}}]`)
	runner := &scriptedCommandRunner{run: func(_ string, _ []string, _ []byte) (commandOutput, error) {
		return commandOutput{stdout: append([]byte(nil), foreignList...)}, nil
	}}
	cli := newAppleCLIWithRunner(runner)
	if err := cli.VerifyAbsent(context.Background(), strings.Repeat("1", 64)); !errors.Is(err, ErrForeignResource) {
		t.Fatalf("foreign fixed container error = %v", err)
	}

	directory := shortSocketTestDirectory(t)
	spec, listener := liveStartSpec(t, directory)
	defer listener.Close()
	runner = &scriptedCommandRunner{run: func(_ string, _ []string, _ []byte) (commandOutput, error) {
		return commandOutput{stdout: []byte("[]")}, nil
	}}
	cli = newAppleCLIWithRunner(runner)
	cli.socketDirectory = directory
	cli.probePort = func() error { return errors.New("occupied") }
	if _, err := cli.Start(context.Background(), spec); !errors.Is(err, ErrForeignResource) {
		t.Fatalf("occupied port error = %v", err)
	}
	if len(runner.calls) != 1 || runner.calls[0].arguments[0] != "list" {
		t.Fatalf("occupied port reached container mutation: %#v", runner.calls)
	}
}

type recordedCommand struct {
	executable string
	arguments  []string
	stdin      []byte
}

type scriptedCommandRunner struct {
	mu    sync.Mutex
	calls []recordedCommand
	run   func(string, []string, []byte) (commandOutput, error)
}

func (r *scriptedCommandRunner) Run(_ context.Context, executable string, arguments []string, stdin io.Reader, _ int64) (commandOutput, error) {
	var input []byte
	if stdin != nil {
		input, _ = io.ReadAll(stdin)
	}
	r.mu.Lock()
	r.calls = append(r.calls, recordedCommand{executable: executable, arguments: append([]string(nil), arguments...), stdin: append([]byte(nil), input...)})
	r.mu.Unlock()
	if r.run == nil {
		return commandOutput{}, nil
	}
	return r.run(executable, arguments, input)
}

type keychainRecordingRunner struct {
	values    map[string][]byte
	calls     []recordedCommand
	addInputs [][]byte
}

func (r *keychainRecordingRunner) Run(_ context.Context, executable string, arguments []string, stdin io.Reader, _ int64) (commandOutput, error) {
	var input []byte
	if stdin != nil {
		input, _ = io.ReadAll(stdin)
	}
	r.calls = append(r.calls, recordedCommand{executable: executable, arguments: append([]string(nil), arguments...), stdin: append([]byte(nil), input...)})
	service := argumentValue(arguments, "-s")
	switch arguments[0] {
	case "find-generic-password":
		value, ok := r.values[service]
		if !ok {
			return commandOutput{}, ErrUnavailable
		}
		return commandOutput{stdout: append(append([]byte(nil), value...), '\n')}, nil
	case "add-generic-password":
		value := append([]byte(nil), bytes.TrimSpace(input)...)
		r.values[service] = value
		r.addInputs = append(r.addInputs, append([]byte(nil), input...))
		return commandOutput{}, nil
	default:
		return commandOutput{}, ErrUnavailable
	}
}

func argumentValue(arguments []string, name string) string {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == name {
			return arguments[index+1]
		}
	}
	return ""
}

func testSecrets() Secrets {
	return Secrets{
		APIToken:   []byte("AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"),
		AdminToken: []byte("AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI"),
	}
}

func shortSocketTestDirectory(t *testing.T) string {
	t.Helper()
	directory := filepath.Clean(os.TempDir())
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		t.Fatalf("test temp directory is not owner-only: %q %v", directory, err)
	}
	return directory
}

func readRawFrame(t *testing.T, reader io.Reader) []byte {
	t.Helper()
	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		t.Fatal(err)
	}
	length := binary.BigEndian.Uint32(header)
	if length == 0 || length > maximumBootstrapFrameBytes {
		t.Fatalf("frame length = %d", length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func writeRawFrame(t *testing.T, writer io.Writer, payload []byte) {
	t.Helper()
	if err := writeAll(writer, framed(payload)); err != nil {
		t.Fatal(err)
	}
}

func framed(payload []byte) []byte {
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(payload)))
	copy(frame[4:], payload)
	return frame
}

func readbackStartSpec(t *testing.T, socketDirectory string) StartSpec {
	t.Helper()
	root := t.TempDir()
	statePath := filepath.Join(root, "homes", "generation-0001")
	if err := os.MkdirAll(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	return StartSpec{
		InstallationID: strings.Repeat("1", 64), OperationID: strings.Repeat("2", 64),
		ImageReference: "ghcr.io/novelkr/opencodex-runtime@sha256:" + strings.Repeat("3", 64),
		IndexDigest:    "sha256:" + strings.Repeat("3", 64), ARM64Digest: "sha256:" + strings.Repeat("4", 64),
		StatePath: statePath, SocketPath: filepath.Join(socketDirectory, "b-"+strings.Repeat("5", 32)),
		Generation: 1, ManifestSHA256: strings.Repeat("6", 64),
	}
}

func liveStartSpec(t *testing.T, directory string) (StartSpec, *net.UnixListener) {
	t.Helper()
	spec := readbackStartSpec(t, directory)
	random, err := randomHex(16)
	if err != nil {
		t.Fatal(err)
	}
	spec.SocketPath = filepath.Join(directory, "b-"+random)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: spec.SocketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(spec.SocketPath, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(spec.SocketPath) })
	return spec, listener
}

func inspectedContainer(spec StartSpec) map[string]any {
	return map[string]any{
		"id": ContainerName,
		"status": map[string]any{
			"state": "running", "networks": []any{},
		},
		"configuration": map[string]any{
			"id": ContainerName,
			"image": map[string]any{
				"reference":  spec.ImageReference,
				"descriptor": map[string]any{"digest": spec.IndexDigest},
			},
			"labels":   ownedLabels(spec),
			"readOnly": true, "useInit": true, "capDrop": []string{"ALL"},
			"capAdd": []string{}, "publishedSockets": []string{},
			"ssh": false, "rosetta": false, "virtualization": false,
			"runtimeHandler": "container-runtime-linux",
			"platform":       map[string]any{"os": "linux", "architecture": "arm64"},
			"resources":      map[string]any{"cpus": 2, "memoryInBytes": 1 << 30},
			"initProcess": map[string]any{
				"terminal": false, "supplementalGroups": []int{},
				"user": map[string]any{"id": map[string]any{"uid": os.Geteuid(), "gid": os.Getegid()}},
			},
			"mounts": []map[string]any{
				{"source": spec.StatePath, "target": GuestStatePath},
				{"source": spec.SocketPath, "target": GuestBootstrapSocket},
				{"type": "tmpfs", "target": "/tmp"},
			},
			"publishedPorts": []map[string]any{{
				"hostAddress": "127.0.0.1", "hostPort": HostServicePort,
				"containerPort": GuestServicePort, "proto": "tcp", "count": 1,
			}},
		},
	}
}

func osUID() string { return strconv.Itoa(os.Geteuid()) }
func osGID() string { return strconv.Itoa(os.Getegid()) }

package daemon

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// The daemon pushes with an ed25519 key of its own, generated at its
// first start and kept in its state directory
// (docs/adr/2026-10-10-daemon-push-identity.md).

// sshKeyName is the file name of the daemon's private key in its state
// directory; the public key is beside it with ".pub" appended.
const sshKeyName = "ssh_ed25519"

const sshEd25519 = "ssh-ed25519"

// sshKey is the daemon's ssh key on disk.
type sshKey struct {
	// Path is the private key's file.
	Path string
	// Blob is the base64 wire encoding of the public key, the second
	// field of its authorized_keys line.
	Blob string
}

// loadOrCreateSSHKey returns the daemon's ssh key in stateDir. Without a
// private key file it generates a pair: the private key in OpenSSH's own
// format with mode 0600, and the public key as an authorized_keys line
// with the comment orchestrator@<daemon> and mode 0644. A private key
// without its public key file is an error, so that a key the owner may
// have registered is never replaced.
func loadOrCreateSSHKey(stateDir string, daemon protocol.DaemonID) (sshKey, error) {
	path := filepath.Join(stateDir, sshKeyName)
	pubPath := path + ".pub"
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if err := writeSSHKey(path, pubPath, "orchestrator@"+string(daemon)); err != nil {
			return sshKey{}, fmt.Errorf("generate the daemon's ssh key: %w", err)
		}
	} else if err != nil {
		return sshKey{}, fmt.Errorf("the daemon's ssh key: %w", err)
	}
	line, err := os.ReadFile(pubPath)
	if err != nil {
		return sshKey{}, fmt.Errorf("the daemon's ssh public key: %w; restore it, or delete %s to generate a new key", err, path)
	}
	blob, err := parseEd25519AuthorizedKey(string(line))
	if err != nil {
		return sshKey{}, fmt.Errorf("the daemon's ssh public key %s: %w", pubPath, err)
	}
	return sshKey{Path: path, Blob: blob}, nil
}

// writeSSHKey generates an ed25519 pair and writes it to path and
// pubPath, leaving neither behind when it fails.
func writeSSHKey(path, pubPath, comment string) error {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	encoded, err := marshalOpenSSHEd25519(private, comment)
	if err != nil {
		return err
	}
	if err := writeNewFile(path, encoded, 0o600); err != nil {
		return err
	}
	line := sshEd25519 + " " + base64.StdEncoding.EncodeToString(ed25519WirePublic(public)) + " " + comment + "\n"
	if err := os.WriteFile(pubPath, []byte(line), 0o644); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

func writeNewFile(path string, data []byte, perm os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}

// ed25519WirePublic is the ssh wire encoding of an ed25519 public key
// (RFC 8709, section 4): the string "ssh-ed25519", then the key.
func ed25519WirePublic(public ed25519.PublicKey) []byte {
	var b bytes.Buffer
	writeSSHString(&b, []byte(sshEd25519))
	writeSSHString(&b, public)
	return b.Bytes()
}

// writeSSHString writes s as an ssh string: its length as a 32-bit big
// endian integer, then its bytes (RFC 4251, section 5).
func writeSSHString(b *bytes.Buffer, s []byte) {
	b.Write(binary.BigEndian.AppendUint32(nil, uint32(len(s))))
	b.Write(s)
}

// marshalOpenSSHEd25519 encodes private, unencrypted, in OpenSSH's own
// private key format, "openssh-key-v1" (PROTOCOL.key in OpenSSH's
// sources). The standard library has no encoder for it. PKCS#8, which it
// has, is not read by every OpenSSH: macOS's, built with LibreSSL, does
// not read an ed25519 key in it.
func marshalOpenSSHEd25519(private ed25519.PrivateKey, comment string) ([]byte, error) {
	var check [4]byte
	if _, err := rand.Read(check[:]); err != nil {
		return nil, err
	}
	public := private.Public().(ed25519.PublicKey)
	var keys bytes.Buffer
	keys.Write(check[:])
	keys.Write(check[:])
	writeSSHString(&keys, []byte(sshEd25519))
	writeSSHString(&keys, public)
	// OpenSSH keeps the 32-byte seed followed by the public key, as Go
	// does.
	writeSSHString(&keys, private)
	writeSSHString(&keys, []byte(comment))
	// Unencrypted keys are padded to the cipher "none"'s block size, 8,
	// with the bytes 1, 2, 3 and so on.
	for pad := byte(1); keys.Len()%8 != 0; pad++ {
		keys.WriteByte(pad)
	}

	var out bytes.Buffer
	out.WriteString("openssh-key-v1\x00")
	writeSSHString(&out, []byte("none"))
	writeSSHString(&out, []byte("none"))
	writeSSHString(&out, nil)
	out.Write(binary.BigEndian.AppendUint32(nil, 1))
	writeSSHString(&out, ed25519WirePublic(public))
	writeSSHString(&out, keys.Bytes())
	return pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: out.Bytes()}), nil
}

// parseEd25519AuthorizedKey returns the base64 key of an authorized_keys
// line of an ed25519 key, "ssh-ed25519 <base64> [comment]", after
// checking that the base64 decodes to an ed25519 public key.
func parseEd25519AuthorizedKey(line string) (string, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != sshEd25519 {
		return "", errors.New("not an ssh-ed25519 key line")
	}
	wire, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return "", fmt.Errorf("the key's base64: %w", err)
	}
	if len(wire) < ed25519.PublicKeySize || !bytes.Equal(wire, ed25519WirePublic(wire[len(wire)-ed25519.PublicKeySize:])) {
		return "", errors.New("not an ed25519 public key")
	}
	return fields[1], nil
}

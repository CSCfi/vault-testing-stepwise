package stepwise

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
)

const pluginPrefix = "vault-plugin-"
const golangImage = "golang:1.20-alpine" // Must be Alpine linux for compatibility with the vault image

// CompilePlugin is a helper method to compile a source plugin
func CompilePlugin(name, pluginName, srcDir, tmpDir string) (string, string, string, error) {
	binName := name
	if !strings.HasPrefix(binName, pluginPrefix) {
		binName = fmt.Sprintf("%s%s", pluginPrefix, binName)
	}
	binPath := path.Join(tmpDir, binName)

	cmd := exec.Command("docker", "run", "--rm", "-v", fmt.Sprintf("%s:%s", tmpDir, tmpDir), "-v", fmt.Sprintf("%s/..:/usr/src/myapp", srcDir), "-w", "/usr/src/myapp", golangImage, "go", "build", "-v", "-o", binPath, path.Join("/usr/src/myapp", pluginName, fmt.Sprintf("cmd/%s/main.go", pluginName)))
	cmd.Stdout = &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	cmd.Stderr = errOut

	// match the target architecture of the docker container
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64")
	if err := cmd.Run(); err != nil {
		// if err here is not nil, it's typically a generic "exit status 1" error
		// message. Return the stderr instead
		return "", "", "", errors.New(errOut.String())
	}

	// calculate sha256
	f, err := os.Open(binPath)
	if err != nil {
		return "", "", "", err
	}

	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", "", "", err
	}

	sha256value := fmt.Sprintf("%x", h.Sum(nil))

	return binName, binPath, sha256value, nil
}

// ReloadFunc are functions that are called when a reload is requested
type ReloadFunc func() error

// CertificateGetter satisfies ReloadFunc and its GetCertificate method
// satisfies the tls.GetCertificate function signature.  Currently it does not
// allow changing paths after the fact.
type CertificateGetter struct {
	sync.RWMutex

	cert *tls.Certificate

	certFile   string
	keyFile    string
	passphrase string
}

func NewCertificateGetter(certFile, keyFile, passphrase string) *CertificateGetter {
	return &CertificateGetter{
		certFile:   certFile,
		keyFile:    keyFile,
		passphrase: passphrase,
	}
}

func (cg *CertificateGetter) Reload() error {
	certPEMBlock, err := ioutil.ReadFile(cg.certFile)
	if err != nil {
		return err
	}
	keyPEMBlock, err := ioutil.ReadFile(cg.keyFile)
	if err != nil {
		return err
	}

	// Check for encrypted pem block
	keyBlock, _ := pem.Decode(keyPEMBlock)
	if keyBlock == nil {
		return errors.New("decoded PEM is blank")
	}

	if x509.IsEncryptedPEMBlock(keyBlock) { //nolint:staticcheck
		keyBlock.Bytes, err = x509.DecryptPEMBlock(keyBlock, []byte(cg.passphrase)) //nolint:staticcheck
		if err != nil {
			return fmt.Errorf("decrypting PEM block failed %s", err)
		}
		keyPEMBlock = pem.EncodeToMemory(keyBlock)
	}

	cert, err := tls.X509KeyPair(certPEMBlock, keyPEMBlock)
	if err != nil {
		return err
	}

	cg.Lock()
	defer cg.Unlock()

	cg.cert = &cert

	return nil
}

func (cg *CertificateGetter) GetCertificate(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	cg.RLock()
	defer cg.RUnlock()

	if cg.cert == nil {
		return nil, fmt.Errorf("nil certificate")
	}

	return cg.cert, nil
}

//go:build linux || freebsd

package integration

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "go.podman.io/podman/v6/test/utils"
)

var _ = Describe("Podman save", func() {
	It("podman save output flag", func() {
		outfile := filepath.Join(podmanTest.TempDir, "alpine.tar")

		save := podmanTest.Podman([]string{"save", "-q", "-o", outfile, ALPINE})
		save.WaitWithDefaultTimeout()
		Expect(save).Should(ExitCleanly())
	})

	It("podman save signature-policy flag", func() {
		SkipIfRemote("--signature-policy N/A for remote")
		outfile := filepath.Join(podmanTest.TempDir, "alpine.tar")

		save := podmanTest.Podman([]string{"save", "-q", "--signature-policy", createPolicyJSONFile(), "-o", outfile, ALPINE})
		save.WaitWithDefaultTimeout()
		Expect(save).Should(ExitCleanly())
	})

	It("podman save oci flag", func() {
		outfile := filepath.Join(podmanTest.TempDir, "alpine.tar")

		save := podmanTest.Podman([]string{"save", "-q", "-o", outfile, "--format", "oci-archive", ALPINE})
		save.WaitWithDefaultTimeout()
		Expect(save).Should(ExitCleanly())
	})

	It("podman save with stdout", func() {
		Skip("Pipe redirection in ginkgo probably won't work")
		outfile := filepath.Join(podmanTest.TempDir, "alpine.tar")

		save := podmanTest.Podman([]string{"save", ALPINE, ">", outfile})
		save.WaitWithDefaultTimeout()
		Expect(save).Should(ExitCleanly())
	})

	It("podman save quiet flag", func() {
		outfile := filepath.Join(podmanTest.TempDir, "alpine.tar")

		save := podmanTest.Podman([]string{"save", "-q", "-o", outfile, ALPINE})
		save.WaitWithDefaultTimeout()
		Expect(save).Should(ExitCleanly())
	})

	It("podman save bogus image", func() {
		outfile := filepath.Join(podmanTest.TempDir, "alpine.tar")

		save := podmanTest.Podman([]string{"save", "-q", "-o", outfile, "FOOBAR"})
		save.WaitWithDefaultTimeout()
		Expect(save).To(ExitWithError(125, "repository name must be lowercase"))
	})

	It("podman save to directory with oci format", func() {
		outdir := filepath.Join(podmanTest.TempDir, "save")

		save := podmanTest.Podman([]string{"save", "-q", "--format", "oci-dir", "-o", outdir, ALPINE})
		save.WaitWithDefaultTimeout()
		Expect(save).Should(ExitCleanly())

		// Smoke test if it looks like an OCI dir
		Expect(filepath.Join(outdir, "oci-layout")).Should(BeAnExistingFile())
		Expect(filepath.Join(outdir, "index.json")).Should(BeAnExistingFile())
		Expect(filepath.Join(outdir, "blobs")).Should(BeAnExistingFile())
	})

	It("podman save to directory with v2s2 docker format", func() {
		outdir := filepath.Join(podmanTest.TempDir, "save")

		save := podmanTest.Podman([]string{"save", "-q", "--format", "docker-dir", "-o", outdir, ALPINE})
		save.WaitWithDefaultTimeout()
		Expect(save).Should(ExitCleanly())

		// Smoke test if it looks like a docker dir
		Expect(filepath.Join(outdir, "version")).Should(BeAnExistingFile())
	})

	It("podman save to directory with docker format and compression", func() {
		outdir := filepath.Join(podmanTest.TempDir, "save")

		save := podmanTest.Podman([]string{"save", "-q", "--compress", "--format", "docker-dir", "-o", outdir, ALPINE})
		save.WaitWithDefaultTimeout()
		Expect(save).Should(ExitCleanly())
	})

	It("podman save to directory with --compress but not use docker-dir and oci-dir", func() {
		outdir := filepath.Join(podmanTest.TempDir, "save")

		save := podmanTest.Podman([]string{"save", "-q", "--compress", "--format", "docker-archive", "-o", outdir, ALPINE})
		save.WaitWithDefaultTimeout()
		Expect(save).To(ExitWithError(125, "--compress can only be set when --format is 'docker-dir'"))

		save = podmanTest.Podman([]string{"save", "-q", "--compress", "--format", "oci-archive", "-o", outdir, ALPINE})
		save.WaitWithDefaultTimeout()
		Expect(save).To(ExitWithError(125, "--compress can only be set when --format is 'docker-dir'"))
	})

	It("podman save bad filename", func() {
		outdir := filepath.Join(podmanTest.TempDir, "save:colon")

		save := podmanTest.Podman([]string{"save", "-q", "--compress", "--format", "docker-dir", "-o", outdir, ALPINE})
		save.WaitWithDefaultTimeout()
		Expect(save).To(ExitWithError(125, fmt.Sprintf(`invalid filename (should not contain ':') "%s"`, outdir)))
	})

	It("podman save remove signature", func() {
		podmanTest.AddImageToRWStore(ALPINE)
		SkipIfRootless("FIXME: Need get in rootless push sign")
		SkipIfRemote("Signing is not supported for remote clients")
		if runtime.GOARCH == "ppc64le" {
			Skip("No registry image for ppc64le")
		}
		tempGNUPGHOME := filepath.Join(podmanTest.TempDir, "tmpGPG")
		err := os.Mkdir(tempGNUPGHOME, os.ModePerm)
		Expect(err).ToNot(HaveOccurred())
		origGNUPGHOME := os.Getenv("GNUPGHOME")
		err = os.Setenv("GNUPGHOME", tempGNUPGHOME)
		Expect(err).ToNot(HaveOccurred())
		defer os.Setenv("GNUPGHOME", origGNUPGHOME)

		port := 5005
		portlock := GetPortLock(strconv.Itoa(port))
		defer portlock.Unlock()

		session := podmanTest.Podman([]string{"run", "-d", "--name", "registry", "-p", strings.Join([]string{strconv.Itoa(port), "5000"}, ":"), REGISTRY_IMAGE})
		session.WaitWithDefaultTimeout()
		Expect(session).Should(ExitCleanly())
		if !WaitContainerReady(podmanTest, "registry", "listening on", 20, 1) {
			Skip("Cannot start docker registry.")
		}

		cmd := exec.Command("gpg", "--import", "sign/secret-key.asc")
		cmd.Stdout = GinkgoWriter
		cmd.Stderr = GinkgoWriter
		err = cmd.Run()
		Expect(err).ToNot(HaveOccurred())

		defaultYaml := filepath.Join(podmanTest.TempDir, "default.yaml")
		cmd = exec.Command("cp", "/etc/containers/registries.d/default.yaml", defaultYaml)
		if err = cmd.Run(); err != nil {
			Skip("no signature store to verify")
		}
		defer func() {
			cmd = exec.Command("cp", defaultYaml, "/etc/containers/registries.d/default.yaml")
			err := cmd.Run()
			Expect(err).ToNot(HaveOccurred())
		}()

		keyPath := filepath.Join(podmanTest.TempDir, "key.gpg")
		cmd = exec.Command("cp", "sign/key.gpg", keyPath)
		Expect(cmd.Run()).To(Succeed())
		defer os.Remove(keyPath)

		sigstore := `
default-docker:
  sigstore: file:///var/lib/containers/sigstore
  sigstore-staging: file:///var/lib/containers/sigstore
`
		Expect(os.WriteFile("/etc/containers/registries.d/default.yaml", []byte(sigstore), 0o755)).To(Succeed())

		pushedImage := fmt.Sprintf("localhost:%d/alpine", port)
		session = podmanTest.Podman([]string{"tag", ALPINE, pushedImage})
		session.WaitWithDefaultTimeout()
		Expect(session).Should(ExitCleanly())

		session = podmanTest.Podman([]string{"push", "-q", "--tls-verify=false", "--sign-by", "foo@bar.com", pushedImage})
		session.WaitWithDefaultTimeout()
		Expect(session).Should(ExitCleanly())

		session = podmanTest.Podman([]string{"rmi", ALPINE, pushedImage})
		session.WaitWithDefaultTimeout()
		Expect(session).Should(ExitCleanly())

		if !IsRemote() {
			// Generate a signature verification policy file
			policyPath := generatePolicyFile(podmanTest.TempDir, port, "testdata/sequoia-key.pub")
			defer os.Remove(policyPath)

			session = podmanTest.Podman([]string{"pull", "-q", "--tls-verify=false", "--signature-policy", policyPath, pushedImage})
			session.WaitWithDefaultTimeout()
			Expect(session).Should(ExitCleanly())

			outfile := filepath.Join(podmanTest.TempDir, "temp.tar")
			save := podmanTest.Podman([]string{"save", "-q", "remove-signatures=true", "-o", outfile, pushedImage})
			save.WaitWithDefaultTimeout()
			Expect(save).To(ExitWithError(125, "invalid reference format"))
		}
	})

	It("podman save image with digest reference", func() {
		// pull a digest reference
		session := podmanTest.Podman([]string{"pull", "-q", ALPINELISTDIGEST})
		session.WaitWithDefaultTimeout()
		Expect(session).Should(ExitCleanly())

		// save a digest reference should exit without error.
		outfile := filepath.Join(podmanTest.TempDir, "temp.tar")
		save := podmanTest.Podman([]string{"save", "-q", "-o", outfile, ALPINELISTDIGEST})
		save.WaitWithDefaultTimeout()
		Expect(save).Should(ExitCleanly())
	})

	It("podman save --multi-image-archive (tagged images)", func() {
		multiImageSave(podmanTest, RESTORE_IMAGES)
	})

	It("podman save --multi-image-archive (untagged images)", func() {
		// #14468: to make execution time more predictable, save at
		// most three images and sort them by size.
		session := podmanTest.Podman([]string{"images", "--sort", "size", "--format", "{{.ID}}"})
		session.WaitWithDefaultTimeout()
		Expect(session).Should(ExitCleanly())
		ids := session.OutputToStringArray()

		Expect(len(ids)).To(BeNumerically(">", 1), "We need to have *some* images to save")
		if len(ids) > 3 {
			ids = ids[:3]
		}
		multiImageSave(podmanTest, ids)
	})
	It("podman save --no-prefix", func() {
		localName := "no-prefix-test:latest"
		tag := podmanTest.Podman([]string{"tag", ALPINE, localName})
		tag.WaitWithDefaultTimeout()
		Expect(tag).Should(ExitCleanly())

		// Sanity check: without the option the image is recorded under
		// the implicit localhost registry.
		outfile := filepath.Join(podmanTest.TempDir, "prefixed.tar")
		save := podmanTest.Podman([]string{"save", "-q", "-o", outfile, localName})
		save.WaitWithDefaultTimeout()
		Expect(save).Should(ExitCleanly())
		Expect(dockerArchiveRepoTags(outfile)).To(ConsistOf("localhost/" + localName))

		outfile = filepath.Join(podmanTest.TempDir, "no-prefix.tar")
		save = podmanTest.Podman([]string{"save", "-q", "--no-prefix", "-o", outfile, localName})
		save.WaitWithDefaultTimeout()
		Expect(save).Should(ExitCleanly())
		Expect(dockerArchiveRepoTags(outfile)).To(ConsistOf(localName))
	})

	It("podman save --no-prefix leaves a qualified name alone", func() {
		outfile := filepath.Join(podmanTest.TempDir, "qualified.tar")
		save := podmanTest.Podman([]string{"save", "-q", "--no-prefix", "-o", outfile, ALPINE})
		save.WaitWithDefaultTimeout()
		Expect(save).Should(ExitCleanly())
		Expect(dockerArchiveRepoTags(outfile)).To(ConsistOf(ALPINE))
	})

	It("podman save --no-prefix oci-archive", func() {
		localName := "no-prefix-oci-test:latest"
		tag := podmanTest.Podman([]string{"tag", ALPINE, localName})
		tag.WaitWithDefaultTimeout()
		Expect(tag).Should(ExitCleanly())

		outfile := filepath.Join(podmanTest.TempDir, "no-prefix-oci.tar")
		save := podmanTest.Podman([]string{"save", "-q", "--format", "oci-archive", "--no-prefix", "-o", outfile, localName})
		save.WaitWithDefaultTimeout()
		Expect(save).Should(ExitCleanly())
		Expect(ociArchiveRefNames(outfile)).To(ConsistOf(localName))
	})
})

// Create a multi-image archive, remove all images, load it and
// make sure that all images are (again) present.
func multiImageSave(podmanTest *PodmanTestIntegration, images []string) {
	// Create the archive.
	outfile := filepath.Join(podmanTest.TempDir, "temp.tar")
	session := podmanTest.Podman(append([]string{"save", "-q", "-o", outfile, "--multi-image-archive"}, images...))
	session.WaitWithDefaultTimeout()
	Expect(session).Should(ExitCleanly())

	// Remove all images.
	session = podmanTest.Podman([]string{"rmi", "-af"})
	session.WaitWithDefaultTimeout()
	Expect(session).Should(ExitCleanly())

	// Now load the archive.
	session = podmanTest.Podman([]string{"load", "-q", "-i", outfile})
	session.WaitWithDefaultTimeout()
	Expect(session).Should(ExitCleanly())
	// Grep for each image in the `podman load` output.
	for _, image := range images {
		Expect(session.OutputToString()).To(ContainSubstring(image))
	}

	// Make sure that each image has really been loaded.
	for _, image := range images {
		session = podmanTest.Podman([]string{"image", "exists", image})
		session.WaitWithDefaultTimeout()
		Expect(session).Should(ExitCleanly())
	}
}

// readFileFromTar returns the contents of the named file in the tar archive at
// tarPath.
func readFileFromTar(tarPath, name string) []byte {
	GinkgoHelper()

	f, err := os.Open(tarPath)
	Expect(err).ToNot(HaveOccurred())
	defer f.Close()

	reader := tar.NewReader(f)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		Expect(err).ToNot(HaveOccurred())
		if header.Name == name {
			contents, err := io.ReadAll(reader)
			Expect(err).ToNot(HaveOccurred())
			return contents
		}
	}
	Fail(fmt.Sprintf("%s not found in %s", name, tarPath))
	return nil
}

// dockerArchiveRepoTags returns the RepoTags of the single image in the
// docker-archive at tarPath.
func dockerArchiveRepoTags(tarPath string) []string {
	GinkgoHelper()

	var manifest []struct {
		RepoTags []string
	}
	Expect(json.Unmarshal(readFileFromTar(tarPath, "manifest.json"), &manifest)).To(Succeed())
	Expect(manifest).To(HaveLen(1))
	return manifest[0].RepoTags
}

// ociArchiveRefNames returns the org.opencontainers.image.ref.name annotation
// of every manifest in the oci-archive at tarPath.
func ociArchiveRefNames(tarPath string) []string {
	GinkgoHelper()

	var index struct {
		Manifests []struct {
			Annotations map[string]string
		}
	}
	Expect(json.Unmarshal(readFileFromTar(tarPath, "index.json"), &index)).To(Succeed())

	names := []string{}
	for _, m := range index.Manifests {
		names = append(names, m.Annotations["org.opencontainers.image.ref.name"])
	}
	return names
}

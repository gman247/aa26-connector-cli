package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// cmdPackage produces a deployable connector tarball. It:
//   1. Validates connector.yaml against the schema (same code as `validate`)
//   2. Runs `docker save <image>:<tag> -o image.tar`
//   3. Tars connector.yaml + image.tar (+ any optional assets/ icon README)
//      into <name>-<version>.tar.gz
//
// The output is what the upload UI accepts via "Add New Source".
func cmdPackage(args []string) error {
	manifestPath := "connector.yaml"
	out := ""
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--out="):
			out = strings.TrimPrefix(a, "--out=")
		case !strings.HasPrefix(a, "-"):
			manifestPath = a
		}
	}

	// 1. Validate.
	if err := cmdValidate([]string{manifestPath}); err != nil {
		return fmt.Errorf("manifest validation failed; fix this before packaging: %w", err)
	}

	// 2. Read enough of the manifest to know name/version/image-ref.
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	jsonBytes, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return fmt.Errorf("yaml parse: %w", err)
	}
	var m struct {
		Metadata struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"metadata"`
		Spec struct {
			Image struct {
				Repository string `json:"repository"`
			} `json:"image"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(jsonBytes, &m); err != nil {
		return err
	}
	if m.Metadata.Name == "" || m.Metadata.Version == "" {
		return errors.New("manifest is missing metadata.name or metadata.version")
	}

	// metadata.version is the single source of truth for the image tag.
	// Authors don't declare it twice; that historically let the manifest
	// drift from the actually-built image (e.g. dropbox-0.2.1 shipped
	// with v0.2.0's compiled bytes on the dev cluster for a full scan
	// cycle because spec.image.tag stayed on `dev` while metadata.version
	// bumped). The image reference is now derived here.
	tag := m.Metadata.Version
	imageRef := m.Spec.Image.Repository + ":" + tag

	// The bundle filename is the canonical lookup key the upload UI
	// uses to identify the manifest version. <name>-<version>.tar.gz
	// is the only shape that round-trips: a custom --out that ends in
	// e.g. dropbox.tar.gz strips the version from the filename and
	// produces a tarball indistinguishable from any other build.
	expectedName := fmt.Sprintf("%s-%s.tar.gz", m.Metadata.Name, m.Metadata.Version)
	if out == "" {
		out = expectedName
	} else if filepath.Base(out) != expectedName {
		return fmt.Errorf(
			"--out filename %q must be %q (or omit --out to use the default). "+
				"The bundle name encodes the version that the upload UI and registry "+
				"display to operators; a mismatched name installs a connector whose "+
				"on-disk filename, manifest version, and image tag disagree.",
			filepath.Base(out), expectedName,
		)
	}

	// 3. docker save the image into a temp file.
	tmpDir, err := os.MkdirTemp("", "aa26-connector-package-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	imgTar := filepath.Join(tmpDir, "image.tar")
	fmt.Printf("→ docker save %s\n", imageRef)
	if err := runDockerSave(imageRef, imgTar); err != nil {
		return fmt.Errorf("docker save: %w (does the image exist locally? try: docker build -t %s .)", err, imageRef)
	}
	imgInfo, err := os.Stat(imgTar)
	if err != nil {
		return err
	}
	fmt.Printf("  image.tar %.1f MB\n", float64(imgInfo.Size())/(1024*1024))

	// 4. Bundle into a tarball.
	manifestDir := filepath.Dir(manifestPath)
	if manifestDir == "" {
		manifestDir = "."
	}
	if err := buildBundle(out, manifestDir, manifestPath, imgTar); err != nil {
		return err
	}
	bundleInfo, _ := os.Stat(out)
	fmt.Printf("✓ wrote %s (%.1f MB)\n", out, float64(bundleInfo.Size())/(1024*1024))
	fmt.Printf("\nUpload it to /connector-upload/ on your DSPM cluster.\n")
	return nil
}

// runDockerSave wraps `docker save` with the same sudo-fallback the test
// command uses, so authors who haven't joined the docker group on their
// host don't get a confusing permission error. When sudo is needed, the
// resulting file is root-owned — chown back to the calling user so the
// rest of the package code can read it.
func runDockerSave(imageRef, outPath string) error {
	cmd := exec.Command("docker", "save", "-o", outPath, imageRef)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err == nil {
		return nil
	}
	probe := exec.Command("docker", "info")
	probeOut, probeErr := probe.CombinedOutput()
	if probeErr != nil && strings.Contains(string(probeOut), "permission denied") {
		fmt.Fprintln(os.Stderr, "  (docker without sudo failed — retrying with sudo)")
		cmd = exec.Command("sudo", "docker", "save", "-o", outPath, imageRef)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
		// Hand the file back to the calling user; otherwise the bundling
		// step below can't read it.
		uid := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
		if err := exec.Command("sudo", "chown", uid, outPath).Run(); err != nil {
			return fmt.Errorf("chown after sudo save: %w", err)
		}
		return nil
	}
	return fmt.Errorf("docker save exit failure")
}

// buildBundle writes connector.yaml at the tarball root, image.tar
// alongside it, and any optional assets/icon/README from the connector
// directory. Files outside that directory are not included.
func buildBundle(out, manifestDir, manifestPath, imageTarPath string) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	// connector.yaml — at the root of the bundle, regardless of source path.
	if err := addFile(tw, manifestPath, "connector.yaml"); err != nil {
		return err
	}
	// image.tar
	if err := addFile(tw, imageTarPath, "image.tar"); err != nil {
		return err
	}
	// Optional artifacts the manifest may reference (icon) or that are
	// useful to keep with the bundle (README, an assets/ tree).
	for _, opt := range []string{"icon.svg", "icon.png", "README.md"} {
		p := filepath.Join(manifestDir, opt)
		if _, err := os.Stat(p); err == nil {
			if err := addFile(tw, p, opt); err != nil {
				return err
			}
		}
	}
	if assets := filepath.Join(manifestDir, "assets"); dirExists(assets) {
		if err := addDir(tw, assets, "assets"); err != nil {
			return err
		}
	}
	return nil
}

func addFile(tw *tar.Writer, srcPath, archivePath string) error {
	info, err := os.Stat(srcPath)
	if err != nil {
		return err
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = archivePath
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()
	_, err = io.Copy(tw, in)
	return err
}

func addDir(tw *tar.Writer, srcRoot, archiveRoot string) error {
	return filepath.Walk(srcRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		archivePath := filepath.Join(archiveRoot, rel)
		if info.IsDir() {
			hdr, _ := tar.FileInfoHeader(info, "")
			hdr.Name = archivePath + "/"
			return tw.WriteHeader(hdr)
		}
		return addFile(tw, path, archivePath)
	})
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

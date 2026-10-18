package software

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ForgeLibraries prefetches digest-pinned profile dependencies; legacy installers fetch their own libraries.
func ForgeLibraries(installerJar, mc string) (files []File, serverJar string, err error) {
	zr, err := zip.OpenReader(installerJar)
	if err != nil {
		return nil, "", fmt.Errorf("software: open installer: %w", err)
	}
	defer zr.Close()

	var profile struct {
		Spec          *int             `json:"spec"`
		JSON          string           `json:"json"`
		ServerJarPath string           `json:"serverJarPath"`
		Libraries     []forgeLibrary   `json:"libraries"`
		Install       *json.RawMessage `json:"install"`
	}
	if err := readZipJSON(&zr.Reader, "install_profile.json", &profile); err != nil {
		return nil, "", err
	}
	if profile.Install != nil && profile.Spec == nil {
		// Legacy installers expect minecraft_server.<version>.jar in the install directory.
		return nil, "minecraft_server." + mc + ".jar", nil
	}
	libs := profile.Libraries
	if name := strings.TrimPrefix(profile.JSON, "/"); name != "" {
		var version struct {
			Libraries []forgeLibrary `json:"libraries"`
		}
		if err := readZipJSON(&zr.Reader, name, &version); err != nil {
			return nil, "", err
		}
		libs = append(libs, version.Libraries...)
	}
	seen := map[string]bool{}
	for _, l := range libs {
		a := l.Downloads.Artifact
		if a.URL == "" || a.SHA1 == "" || a.Path == "" {
			continue
		}
		clean := path.Clean(a.Path)
		if strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || clean == ".." {
			return nil, "", fmt.Errorf("software: installer library path %q leaves the libraries directory", a.Path)
		}
		if seen[clean] {
			continue
		}
		seen[clean] = true
		d, err := ParseDigest(SHA1, a.SHA1)
		if err != nil {
			return nil, "", fmt.Errorf("software: installer library %s: %w", l.Name, err)
		}
		if err := requireHTTPS(a.URL); err != nil {
			return nil, "", err
		}
		files = append(files, File{Path: path.Join(LibrariesDir, clean), URL: a.URL, Digest: d, Size: a.Size})
	}
	serverJar = "minecraft_server." + mc + ".jar"
	if p := profile.ServerJarPath; p != "" {
		p = strings.ReplaceAll(p, "{LIBRARY_DIR}", LibrariesDir)
		p = strings.ReplaceAll(p, "{MINECRAFT_VERSION}", mc)
		if strings.Contains(p, "{") || strings.HasPrefix(path.Clean(p), "../") || strings.HasPrefix(p, "/") {
			return nil, "", fmt.Errorf("software: installer server jar path %q is not one Felis understands", profile.ServerJarPath)
		}
		serverJar = path.Clean(p)
	}
	return files, serverJar, nil
}

type forgeLibrary struct {
	Name      string `json:"name"`
	Downloads struct {
		Artifact struct {
			Path string `json:"path"`
			URL  string `json:"url"`
			SHA1 string `json:"sha1"`
			Size int64  `json:"size"`
		} `json:"artifact"`
	} `json:"downloads"`
}

func readZipJSON(zr *zip.Reader, name string, v any) error {
	f, err := zr.Open(name)
	if err != nil {
		return fmt.Errorf("software: installer has no %s: %w", name, err)
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, maxMetadataBytes))
	if err != nil {
		return fmt.Errorf("software: read %s: %w", name, err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("software: parse %s: %w", name, err)
	}
	return nil
}

var ErrNoForgeLaunch = errors.New("software: the installer left no run script or server jar")

// ForgeLaunch uses the final run.sh Java command without its forwarded shell arguments; legacy installers launch a single jar.
func ForgeLaunch(dataDir string, inst LoaderInstaller) (Launch, error) {
	script := filepath.Join(dataDir, "run.sh")
	if f, err := os.Open(script); err == nil {
		defer f.Close()
		var args []string
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimPrefix(strings.TrimSpace(sc.Text()), "exec ")
			if !strings.HasPrefix(line, "java ") {
				continue
			}
			args = args[:0]
			for _, tok := range strings.Fields(line)[1:] {
				if tok == "||" || tok == "&&" || tok == ";" {
					break
				}
				if tok == `"$@"` || tok == "$@" {
					continue
				}
				args = append(args, tok)
			}
		}
		if err := sc.Err(); err != nil {
			return Launch{}, err
		}
		if len(args) > 0 {
			return Launch{Args: args, GameArgs: []string{"nogui"}}, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Launch{}, err
	}
	for _, name := range []string{
		"forge-" + inst.Version + ".jar",
		"forge-" + inst.Version + "-universal.jar",
	} {
		if _, err := os.Stat(filepath.Join(dataDir, name)); err == nil {
			return Launch{Jar: name, GameArgs: []string{"nogui"}}, nil
		}
	}
	return Launch{}, ErrNoForgeLaunch
}

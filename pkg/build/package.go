// Copyright 2022 Chainguard, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package build

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"text/template"

	apkofs "chainguard.dev/apko/pkg/fs"
	"chainguard.dev/apko/pkg/tarball"
	"chainguard.dev/melange/internal/sign"
	"github.com/psanford/memfs"
)

type PackageContext struct {
	Context       *Context
	Origin        *Package
	PackageName   string
	InstalledSize int64
	DataHash      string
	OutDir        string
	Logger        *log.Logger
}

func (pkg *Package) Emit(ctx *PipelineContext) error {
	fakesp := Subpackage{
		Name: pkg.Name,
	}
	return fakesp.Emit(ctx)
}

func (spkg *Subpackage) Emit(ctx *PipelineContext) error {
	pc := PackageContext{
		Context:     ctx.Context,
		Origin:      &ctx.Context.Configuration.Package,
		PackageName: spkg.Name,
		OutDir:      filepath.Join(ctx.Context.OutDir, ctx.Context.Arch.ToAPK()),
		Logger:      log.New(log.Writer(), fmt.Sprintf("melange (%s/%s): ", spkg.Name, ctx.Context.Arch.ToAPK()), log.LstdFlags|log.Lmsgprefix),
	}
	return pc.EmitPackage()
}

func (pc *PackageContext) Identity() string {
	return fmt.Sprintf("%s-%s-r%d", pc.PackageName, pc.Origin.Version, pc.Origin.Epoch)
}

func (pc *PackageContext) Filename() string {
	return fmt.Sprintf("%s/%s.apk", pc.OutDir, pc.Identity())
}

func (pc *PackageContext) WorkspaceSubdir() string {
	return filepath.Join(pc.Context.WorkspaceDir, "melange-out", pc.PackageName)
}

var controlTemplate = `
# Generated by melange.
pkgname = {{.PackageName}}
pkgver = {{.Origin.Version}}-r{{.Origin.Epoch}}
arch = x86_64
size = {{.InstalledSize}}
pkgdesc = {{.Origin.Description}}
{{- range $copyright := .Origin.Copyright }}
license = {{ $copyright.License }}
{{- end }}
{{- range $dep := .Origin.Dependencies.Runtime }}
depend = {{ $dep }}
{{- end }}
datahash = {{.DataHash}}
`

func (pc *PackageContext) GenerateControlData(w io.Writer) error {
	tmpl := template.New("control")
	return template.Must(tmpl.Parse(controlTemplate)).Execute(w, pc)
}

func (pc *PackageContext) SignatureName() string {
	return fmt.Sprintf(".SIGN.RSA.%s.pub", filepath.Base(pc.Context.SigningKey))
}

func combine(out io.Writer, inputs ...io.Reader) error {
	for _, input := range inputs {
		if _, err := io.Copy(out, input); err != nil {
			return err
		}
	}

	return nil
}

// TODO(kaniini): generate APKv3 packages
func (pc *PackageContext) EmitPackage() error {
	pc.Logger.Printf("generating package %s", pc.Identity())

	dataTarGz, err := os.CreateTemp("", "melange-data-*.tar.gz")
	if err != nil {
		return fmt.Errorf("unable to open temporary file for writing: %w", err)
	}
	defer dataTarGz.Close()

	tarctx, err := tarball.NewContext(
		tarball.WithSourceDateEpoch(pc.Context.SourceDateEpoch),
		tarball.WithOverrideUIDGID(0, 0),
		tarball.WithOverrideUname("root"),
		tarball.WithOverrideGname("root"),
		tarball.WithUseChecksums(true),
	)
	if err != nil {
		return fmt.Errorf("unable to build tarball context: %w", err)
	}

	fsys := apkofs.DirFS(pc.WorkspaceSubdir())
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}

		pc.InstalledSize += fi.Size()
		return nil
	}); err != nil {
		return fmt.Errorf("unable to preprocess package data: %w", err)
	}

	// TODO(kaniini): generate so:/cmd: virtuals for the filesystem
	// prepare data.tar.gz
	dataDigest := sha256.New()
	dataMW := io.MultiWriter(dataDigest, dataTarGz)
	if err := tarctx.WriteArchive(dataMW, fsys); err != nil {
		return fmt.Errorf("unable to write data tarball: %w", err)
	}

	pc.DataHash = hex.EncodeToString(dataDigest.Sum(nil))
	pc.Logger.Printf("  data.tar.gz installed-size: %d", pc.InstalledSize)
	pc.Logger.Printf("  data.tar.gz digest: %s", pc.DataHash)

	if _, err := dataTarGz.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("unable to rewind data tarball: %w", err)
	}

	// prepare control.tar.gz
	multitarctx, err := tarball.NewContext(
		tarball.WithSourceDateEpoch(pc.Context.SourceDateEpoch),
		tarball.WithOverrideUIDGID(0, 0),
		tarball.WithOverrideUname("root"),
		tarball.WithOverrideGname("root"),
		tarball.WithSkipClose(true),
	)
	if err != nil {
		return fmt.Errorf("unable to build tarball context: %w", err)
	}

	var controlBuf bytes.Buffer
	if err := pc.GenerateControlData(&controlBuf); err != nil {
		return fmt.Errorf("unable to process control template: %w", err)
	}

	controlFS := memfs.New()
	if err := controlFS.WriteFile(".PKGINFO", controlBuf.Bytes(), 0644); err != nil {
		return fmt.Errorf("unable to build control FS: %w", err)
	}

	controlTarGz, err := os.CreateTemp("", "melange-control-*.tar.gz")
	if err != nil {
		return fmt.Errorf("unable to open temporary file for writing: %w", err)
	}
	defer controlTarGz.Close()

	controlDigest := sha1.New() // nolint:gosec
	controlMW := io.MultiWriter(controlDigest, controlTarGz)
	if err := multitarctx.WriteArchive(controlMW, controlFS); err != nil {
		return fmt.Errorf("unable to write control tarball: %w", err)
	}

	controlHash := hex.EncodeToString(controlDigest.Sum(nil))
	pc.Logger.Printf("  control.tar.gz digest: %s", controlHash)

	if _, err := controlTarGz.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("unable to rewind control tarball: %w", err)
	}

	combinedParts := []io.Reader{controlTarGz, dataTarGz}

	if pc.Context.SigningKey != "" {
		signatureFS := memfs.New()
		signatureBuf, err := sign.RSASignSHA1Digest(controlDigest.Sum(nil),
			pc.Context.SigningKey, pc.Context.SigningPassphrase)
		if err != nil {
			return fmt.Errorf("unable to generate signature: %w", err)
		}

		if err := signatureFS.WriteFile(pc.SignatureName(), signatureBuf, 0644); err != nil {
			return fmt.Errorf("unable to build signature FS: %w", err)
		}

		signatureTarGz, err := os.CreateTemp("", "melange-signature-*.tar.gz")
		if err != nil {
			return fmt.Errorf("unable to open temporary file for writing: %w", err)
		}
		defer signatureTarGz.Close()

		if err := multitarctx.WriteArchive(signatureTarGz, signatureFS); err != nil {
			return fmt.Errorf("unable to write signature tarball: %w", err)
		}

		if _, err := signatureTarGz.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("unable to rewind signature tarball: %w", err)
		}

		combinedParts = append([]io.Reader{signatureTarGz}, combinedParts...)
	}

	// build the final tarball
	if err := os.MkdirAll(pc.OutDir, 0755); err != nil {
		return fmt.Errorf("unable to create output directory: %w", err)
	}

	outFile, err := os.Create(pc.Filename())
	if err != nil {
		return fmt.Errorf("unable to create apk file: %w", err)
	}
	defer outFile.Close()

	if err := combine(outFile, combinedParts...); err != nil {
		return fmt.Errorf("unable to write apk file: %w", err)
	}

	pc.Logger.Printf("wrote %s", outFile.Name())

	return nil
}

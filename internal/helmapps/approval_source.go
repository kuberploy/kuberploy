package helmapps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"
)

const unknownChartDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

func optionalDigest(value string) bool { return value == "" || validDigest(value) }

func (a Approval) CanonicalSource() (ChartSource, error) {
	if a.Source.Kind == "" && a.Source.OCI == nil && a.Source.HelmRepository == nil && a.Source.Git == nil {
		source := ChartSource{Kind: ChartSourceKindOCI, OCI: &OCIChartSource{
			Repository: a.OCIRepository,
			Version:    a.ChartVersion,
			Digest:     a.ManifestDigest,
		}}
		return source, source.Validate()
	}
	if a.Source.Validate() != nil {
		return ChartSource{}, ErrInvalid
	}
	return cloneChartSource(a.Source), nil
}

func approvalRepositoryMatchesSource(repository, chartName string, source ChartSource) bool {
	if source.Validate() != nil || chartName == "" {
		return false
	}
	if source.Kind == ChartSourceKindOCI {
		return source.OCI.Repository == repository
	}
	return repository == syntheticChartRepository(source.Kind, chartName)
}

func syntheticChartRepository(kind ChartSourceKind, chartName string) string {
	return "oci://source.kuberploy.invalid/" + string(kind) + "/" + chartName
}

func cloneChartSource(source ChartSource) ChartSource {
	clone := ChartSource{Kind: source.Kind}
	if source.OCI != nil {
		value := *source.OCI
		clone.OCI = &value
	}
	if source.HelmRepository != nil {
		value := *source.HelmRepository
		clone.HelmRepository = &value
	}
	if source.Git != nil {
		value := *source.Git
		clone.Git = &value
	}
	return clone
}

func marshalChartSource(source ChartSource) ([]byte, error) {
	if source.Validate() != nil {
		return nil, ErrInvalid
	}
	raw, err := json.Marshal(source)
	if err != nil || len(raw) == 0 || len(raw) > 8192 {
		return nil, ErrInvalid
	}
	return raw, nil
}

func unmarshalChartSource(raw []byte) (ChartSource, error) {
	if len(raw) == 0 || len(raw) > 8192 {
		return ChartSource{}, ErrInvalid
	}
	var source ChartSource
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&source) != nil || !errors.Is(decoder.Decode(&struct{}{}), io.EOF) || source.Validate() != nil {
		return ChartSource{}, ErrInvalid
	}
	return source, nil
}

func chartPackageValuesSchemaDigest(packageBytes []byte, chartName string) (string, error) {
	if len(packageBytes) == 0 || len(packageBytes) > MaximumChartSize || !chartSourceNameRE.MatchString(chartName) {
		return "", ErrUnsafeChart
	}
	gzipReader, err := gzip.NewReader(bytes.NewReader(packageBytes))
	if err != nil {
		return "", ErrUnsafeChart
	}
	defer gzipReader.Close() //nolint:errcheck
	gzipReader.Multistream(false)
	tarReader := tar.NewReader(gzipReader)
	files, expanded, digest := 0, int64(0), ""
	for {
		header, nextErr := tarReader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil || header == nil || header.Name == "" || path.IsAbs(header.Name) ||
			strings.Contains(header.Name, "\\") || path.Clean(header.Name) != header.Name ||
			(header.Name != chartName && !strings.HasPrefix(header.Name, chartName+"/")) {
			return "", ErrUnsafeChart
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA || header.Size < 0 || header.Size > MaximumFileSize {
			return "", ErrUnsafeChart
		}
		files++
		expanded += header.Size
		if files > MaximumChartFiles || expanded > MaximumExpandSize {
			return "", ErrUnsafeChart
		}
		if header.Name != chartName+"/values.schema.json" {
			continue
		}
		if digest != "" || header.Size < 2 || header.Size > MaximumSchemaSize {
			return "", ErrUnsafeChart
		}
		content, readErr := io.ReadAll(io.LimitReader(tarReader, MaximumSchemaSize+1))
		if readErr != nil || int64(len(content)) != header.Size {
			return "", ErrUnsafeChart
		}
		digest = digestBytes(content)
	}
	if digest == "" {
		return "", ErrUnsafeChart
	}
	return digest, nil
}

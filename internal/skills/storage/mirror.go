package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const (
	uploadPartSize    = 8 << 20 // 8 MB — skills are small (<10 MB whole tree)
	uploadConcurrency = 3
)

// Mirror persists skill directory trees to S3 and rehydrates them on
// demand. The local filesystem remains the read path; this type is the
// only place that touches S3 for skills.
type Mirror struct {
	client   *s3.Client
	uploader *manager.Uploader
	bucket   string
	prefix   string // already normalised, no trailing slash
}

// NewMirror builds a Mirror from the supplied config. Credentials come
// from the default SDK chain (env vars, ~/.aws/credentials, IAM role on
// EC2/ECS) — same convention as the media S3Backend.
func NewMirror(ctx context.Context, cfg Config) (*Mirror, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("skills storage: bucket is required")
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("skills storage: load aws config: %w", err)
	}

	var s3Opts []func(*s3.Options)
	if cfg.Endpoint != "" {
		ep := cfg.Endpoint
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(ep)
			o.UsePathStyle = true
		})
	}
	client := s3.NewFromConfig(awsCfg, s3Opts...)

	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = uploadPartSize
		u.Concurrency = uploadConcurrency
	})

	return &Mirror{
		client:   client,
		uploader: uploader,
		bucket:   cfg.Bucket,
		prefix:   strings.TrimSuffix(cfg.Prefix, "/"),
	}, nil
}

// SkillKeyPrefix builds the S3 key prefix for one (tenant, slug, version)
// triple. tenantSlug must be non-empty in multi-tenant deployments — the
// caller's responsibility, we just join.
func (m *Mirror) SkillKeyPrefix(tenantSlug, skillSlug string, version int) string {
	return fmt.Sprintf("%s/tenants/%s/skills-store/%s/%d",
		m.prefix, tenantSlug, skillSlug, version)
}

// UploadDir mirrors every file under localDir to S3 under keyPrefix.
// Symlinks and directories don't get their own S3 objects (S3 is flat);
// empty subdirectories are silently dropped. Returns the number of files
// uploaded so callers can log it without an extra walk.
func (m *Mirror) UploadDir(ctx context.Context, localDir, keyPrefix string) (int, error) {
	cleanDir, err := filepath.Abs(localDir)
	if err != nil {
		return 0, fmt.Errorf("skills storage: resolve local dir: %w", err)
	}
	var uploaded int
	err = filepath.WalkDir(cleanDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			// Skip symlinks, sockets, devices — they don't survive a round-trip
			// through S3 and a skill that depends on one is already broken.
			return nil
		}
		rel, relErr := filepath.Rel(cleanDir, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		objKey := keyPrefix + "/" + rel

		f, openErr := os.Open(path)
		if openErr != nil {
			return fmt.Errorf("skills storage: open %s: %w", rel, openErr)
		}
		_, upErr := m.uploader.Upload(ctx, &s3.PutObjectInput{
			Bucket: aws.String(m.bucket),
			Key:    aws.String(objKey),
			Body:   f,
		})
		f.Close()
		if upErr != nil {
			return fmt.Errorf("skills storage: upload %s: %w", rel, upErr)
		}
		uploaded++
		return nil
	})
	if err != nil {
		return uploaded, err
	}
	slog.Info("skills.s3.upload_dir", "key_prefix", keyPrefix, "files", uploaded)
	return uploaded, nil
}

// DownloadDir streams every object under keyPrefix into localDir,
// preserving the relative directory layout. Existing local files are
// overwritten — the caller is responsible for choosing an empty dest.
// Returns the number of files downloaded; a fresh keyPrefix with no
// objects is not an error (the skill might be local-only on this host).
func (m *Mirror) DownloadDir(ctx context.Context, keyPrefix, localDir string) (int, error) {
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return 0, fmt.Errorf("skills storage: mkdir dest: %w", err)
	}
	cleanLocal, err := filepath.Abs(localDir)
	if err != nil {
		return 0, fmt.Errorf("skills storage: resolve local dir: %w", err)
	}

	var downloaded int
	paginator := s3.NewListObjectsV2Paginator(m.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(m.bucket),
		Prefix: aws.String(keyPrefix + "/"),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return downloaded, fmt.Errorf("skills storage: list objects: %w", err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			rel := strings.TrimPrefix(*obj.Key, keyPrefix+"/")
			// Defensive: refuse anything that escapes localDir. S3 keys can
			// in principle contain ".." segments if some external party
			// uploaded malicious objects to our prefix; the tarball
			// extractor already rejects this on install, but a defence in
			// depth is cheap.
			if rel == "" || strings.Contains(rel, "..") {
				continue
			}
			destPath := filepath.Join(cleanLocal, filepath.FromSlash(rel))
			if !strings.HasPrefix(destPath, cleanLocal+string(filepath.Separator)) && destPath != cleanLocal {
				slog.Warn("skills.s3.download_dir.path_escape", "key", *obj.Key, "rel", rel)
				continue
			}
			if err := m.downloadObject(ctx, *obj.Key, destPath); err != nil {
				return downloaded, err
			}
			downloaded++
		}
	}
	slog.Info("skills.s3.download_dir", "key_prefix", keyPrefix, "files", downloaded)
	return downloaded, nil
}

func (m *Mirror) downloadObject(ctx context.Context, objKey, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("skills storage: mkdir for object: %w", err)
	}
	out, err := m.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(m.bucket),
		Key:    aws.String(objKey),
	})
	if err != nil {
		return fmt.Errorf("skills storage: get %s: %w", objKey, err)
	}
	defer out.Body.Close()

	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("skills storage: create %s: %w", destPath, err)
	}
	if _, err := io.Copy(f, out.Body); err != nil {
		f.Close()
		return fmt.Errorf("skills storage: write %s: %w", destPath, err)
	}
	return f.Close()
}

// DeletePrefix removes every object under keyPrefix. Used when a skill
// row is permanently deleted from DB; soft-delete callers should NOT use
// this so the on-disk version stays available for rollback.
func (m *Mirror) DeletePrefix(ctx context.Context, keyPrefix string) error {
	paginator := s3.NewListObjectsV2Paginator(m.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(m.bucket),
		Prefix: aws.String(keyPrefix + "/"),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("skills storage: list for delete: %w", err)
		}
		if len(page.Contents) == 0 {
			continue
		}
		ids := make([]s3types.ObjectIdentifier, 0, len(page.Contents))
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			ids = append(ids, s3types.ObjectIdentifier{Key: obj.Key})
		}
		if len(ids) == 0 {
			continue
		}
		if _, err := m.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(m.bucket),
			Delete: &s3types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		}); err != nil {
			return fmt.Errorf("skills storage: delete objects: %w", err)
		}
	}
	slog.Info("skills.s3.delete_prefix", "key_prefix", keyPrefix)
	return nil
}

// EnsureLocal hydrates a single (slug, version) tree from S3 into localDir
// when the local copy is missing. Idempotent: a populated localDir short-
// circuits without an S3 round-trip. Returns the number of files
// downloaded (0 means "already had it locally"). Used by the startup
// sync to backfill a fresh ASG node and by the runtime miss-handler
// when a file disappears between two reads.
//
// The "populated" check is intentionally lax: any regular file inside
// localDir counts. We assume nothing else writes into the per-version
// dir, so seeing any file means the install completed.
func (m *Mirror) EnsureLocal(ctx context.Context, keyPrefix, localDir string) (int, error) {
	if hasLocalFiles(localDir) {
		return 0, nil
	}
	return m.DownloadDir(ctx, keyPrefix, localDir)
}

// hasLocalFiles returns true when localDir contains at least one regular
// file. Empty dirs and missing dirs both count as "needs download".
func hasLocalFiles(localDir string) bool {
	stat, err := os.Stat(localDir)
	if err != nil || !stat.IsDir() {
		return false
	}
	var found bool
	_ = filepath.WalkDir(localDir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Type().IsRegular() {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// HasObjects probes whether at least one object exists under keyPrefix.
// Used by the startup warm-up to decide whether to attempt a download.
// A missing prefix is not an error — the function returns ok=false, nil.
func (m *Mirror) HasObjects(ctx context.Context, keyPrefix string) (bool, error) {
	out, err := m.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(m.bucket),
		Prefix:  aws.String(keyPrefix + "/"),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return false, fmt.Errorf("skills storage: probe %s: %w", keyPrefix, err)
	}
	return out.KeyCount != nil && *out.KeyCount > 0, nil
}


package workspace

import (
	"context"
	"reflect"
	"time"
)

// ImageScanReader reads Harbor's vulnerability scan of an image (oci.Client.HarborScan); nil without an error = not scanned yet.
type ImageScanReader func(ctx context.Context, image string) (*ImageScan, error)

// SetImageScans adds Harbor's scans to the projects whose image is in the registry at host (the platform's Harbor); without it, or with an empty host, projects have no scan.
func (s *Service) SetImageScans(f ImageScanReader, host string) {
	s.scans, s.scanHost = f, normaliseHost(host)
}

// scanRecheck is how long a finished scan stands before it is read again (Harbor rescans on a new database only when told to; a new scan of the same digest is rare); scanMissingRecheck how long an image without a scan (none yet, or no scanner) waits.
const (
	scanRecheck        = 6 * time.Hour
	scanMissingRecheck = 15 * time.Minute
)

// finishedScan tells whether a scan status is final (no need to look again soon).
func finishedScan(status string) bool {
	return status == "Success" || status == "Error" || status == "Stopped"
}

// refreshScan reads Harbor's scan of the image the project runs (its pinned image, the build from Git or the base), at every check while it runs, every quarter of an hour while there is none and every few hours once it is finished; it tells whether the project changed. An image outside Harbor has none; Harbor not answering keeps the last one.
func (s *Service) refreshScan(ctx context.Context, p *Project) bool {
	if s.scans == nil || s.scanHost == "" {
		return false
	}
	img := s.EffectiveImage(*p)
	if img == "" || imageHost(img) != s.scanHost {
		if p.ImageScan != nil {
			p.ImageScan = nil
			return true
		}
		return false
	}
	key := p.Slug + " " + img
	if last, ok := s.scanRead.Load(key); ok {
		age := s.now().Sub(last.(time.Time))
		switch {
		case p.ImageScan == nil && age < scanMissingRecheck:
			return false
		case p.ImageScan != nil && finishedScan(p.ImageScan.Status) && age < scanRecheck:
			return false
		}
	}
	scan, err := s.scans(ctx, img)
	if err != nil {
		return false
	}
	s.scanRead.Store(key, s.now())
	if reflect.DeepEqual(scan, p.ImageScan) {
		return false
	}
	p.ImageScan = scan
	return true
}

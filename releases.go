package empire

import (
	"database/sql"
	"database/sql/driver"
)

// ReleaseID represents the unique identifier for a Release.
type ReleaseID string

// Scan implements the sql.Scanner interface.
func (r *ReleaseID) Scan(src interface{}) error {
	if src, ok := src.([]byte); ok {
		*r = ReleaseID(src)
	}

	return nil
}

// Value implements the driver.Value interface.
func (r ReleaseID) Value() (driver.Value, error) {
	return driver.Value(string(r)), nil
}

// ReleaseVersion represents the auto incremented human friendly version number of the
// release.
type ReleaseVersion int

// Release is a combination of a Config and a Slug, which form a deployable
// release.
type Release struct {
	ID  ReleaseID      `json:"id" db:"id"`
	Ver ReleaseVersion `json:"version" db:"version"` // Version conflicts with gorps optimistic locking.

	AppName  `json:"-" db:"app_id"`
	ConfigID `json:"-" db:"config_id"`
	SlugID   `json:"-" db:"slug_id"`
}

// ReleaseRepository is an interface that can be implemented for storing and
// retrieving releases.
type ReleasesRepository interface {
	Create(*Release) (*Release, error)
	FindByAppName(AppName) ([]*Release, error)
	Head(AppName) (*Release, error)
}

// NewReleasesRepository is a factory method that returns a new Repository.
func NewReleasesRepository(db DB) (ReleasesRepository, error) {
	return &releasesRepository{db}, nil
}

// dbRelease is a db representation of a release.
type dbRelease struct {
	ID       *string `db:"id"`
	Ver      int64   `db:"version"` // Ver because Version is reserved in gorp for optimistic locking.
	AppID    string  `db:"app_id"`
	ConfigID string  `db:"config_id"`
	SlugID   string  `db:"slug_id"`
}

// releasesRepository is an implementation of the ReleasesRepository interface backed by
// a DB.
type releasesRepository struct {
	DB
}

func (r *releasesRepository) Create(release *Release) (*Release, error) {
	return CreateRelease(r.DB, release)
}

func (r *releasesRepository) Head(appName AppName) (*Release, error) {
	return LastRelease(r.DB, appName)
}

func (r *releasesRepository) FindByAppName(appName AppName) ([]*Release, error) {
	var rs []*Release
	return rs, r.DB.Select(&rs, `select * from releases where app_id = $1 order by version desc limit 1`, string(appName))
}

// CreateRelease creates a new Release and inserts it into the database.
func CreateRelease(db DB, release *Release) (*Release, error) {
	t, err := db.Begin()
	if err != nil {
		return release, err
	}

	// Get the last release version for this app.
	v, err := LastReleaseVersion(t, release.AppName)
	if err != nil {
		return release, err
	}

	// Increment the release version.
	release.Ver = v + 1

	if err := t.Insert(release); err != nil {
		return release, err
	}

	return release, t.Commit()
}

// LastReleaseVersion returns the last ReleaseVersion for the given App. This
// function also ensures that the last release is locked until the transaction
// is commited, so the release version can be incremented atomically.
func LastReleaseVersion(db Queryier, appName AppName) (version ReleaseVersion, err error) {
	err = db.SelectOne(&version, `select version from releases where app_id = $1 order by version desc for update`, string(appName))

	if err == sql.ErrNoRows {
		return 0, nil
	}

	return
}

// LastRelease returns the last Release for the given App.
func LastRelease(db Queryier, appName AppName) (*Release, error) {
	var release Release

	if err := db.SelectOne(&release, `select * from releases where app_id = $1 order by version desc limit 1`, string(appName)); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}

		return nil, err
	}

	return &release, nil
}

// ReleaseesService represents a service for interacting with Releases.
type ReleasesService interface {
	// Create creates a new release.
	Create(*App, *Config, *Slug) (*Release, error)

	// Find existing releases for an app
	FindByApp(*App) ([]*Release, error)

	// Find current release for an app
	Head(*App) (*Release, error)
}

// releasesService is a base implementation of the ReleasesService interface.
type releasesService struct {
	ReleasesRepository
	ProcessesRepository
	Manager
}

// NewReleasesService returns a new ReleasesService instance.
func NewReleasesService(r ReleasesRepository, p ProcessesRepository, m Manager) (ReleasesService, error) {
	return &releasesService{
		ReleasesRepository:  r,
		ProcessesRepository: p,
		Manager:             m,
	}, nil
}

// Create creates the release, then sets the current process formation on the release.
func (s *releasesService) Create(app *App, config *Config, slug *Slug) (*Release, error) {
	r := &Release{
		AppName:  app.Name,
		ConfigID: config.ID,
		SlugID:   slug.ID,
	}

	r, err := s.ReleasesRepository.Create(r)
	if err != nil {
		return r, err
	}

	// Create a new formation for this release.
	formation, err := s.createFormation(r, slug)
	if err != nil {
		return nil, err
	}

	// Schedule the new release onto the cluster.
	if err := s.Manager.ScheduleRelease(r, config, slug, formation); err != nil {
		return r, err
	}

	return r, nil
}

func (s *releasesService) FindByApp(a *App) ([]*Release, error) {
	return s.ReleasesRepository.FindByAppName(a.Name)
}

func (s *releasesService) Head(app *App) (*Release, error) {
	return s.ReleasesRepository.Head(app.Name)
}

func (s *releasesService) createFormation(release *Release, slug *Slug) (Formation, error) {
	// Get the old release, so we can copy the Formation.
	last, err := s.ReleasesRepository.Head(release.AppName)
	if err != nil {
		return nil, err
	}

	var existing Formation

	if last != nil {
		existing, err = s.ProcessesRepository.All(last.ID)
		if err != nil {
			return nil, err
		}
	}

	f := NewFormation(existing, slug.ProcessTypes)

	for _, p := range f {
		p.ReleaseID = release.ID

		if _, err := s.ProcessesRepository.Create(p); err != nil {
			return f, err
		}
	}

	return f, nil
}

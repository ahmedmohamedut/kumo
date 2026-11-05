package dbinstance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ahmedmohamedut/kumo/internal/id"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/dbcred"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// CreateInput is a POST /rds/db-instances body with the calling user attached.
//
// AdminPassword is the only field that is not kept anywhere. It is turned into
// a verifier and dropped before this struct goes out of scope, and it is never
// logged, stored or put in the task payload (docs/design/07-databases.md R8).
type CreateInput struct {
	Identifier string
	Engine     Engine
	// EngineVersion may be empty, which takes the newest available version of
	// the engine.
	EngineVersion string
	InstanceType  string
	StorageGB     int
	// SubnetID may be empty, which attaches the server to the default subnet
	// the way a launch does.
	SubnetID      string
	AdminUsername string
	AdminPassword string
	DatabaseName  string
	CreatedBy     string
}

var (
	// identifierPattern is a DNS label: the identifier becomes the guest's
	// name, the static DHCP host's name and so the endpoint (R9).
	identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	// namePattern is an unquoted Postgres identifier, which is what lets the
	// role and the database go into the first-boot SQL without quoting rules
	// of their own.
	namePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	// versionPattern is a major version: one engine release line.
	versionPattern = regexp.MustCompile(`^\d{1,3}$`)
)

// reservedNames may not be the admin role or the initial database. The pg_
// prefix is reserved by Postgres itself; the rest are names a user would only
// pick by accident or to collide with something
// (docs/design/07-databases.md section 3).
var reservedNames = map[string]struct{}{
	"postgres":  {},
	"template0": {},
	"template1": {},
	"rdsadmin":  {},
	"kumo":      {},
}

// Create records the request and queues the work: the handler answers 202 with
// the DB instance `creating` and never waits for a boot
// (docs/design/07-databases.md section 4).
//
// The order is deliberate. Everything that can refuse the request happens
// before anything is written: the fields, the engine image, the instance type
// and the capacity. Then one transaction claims the address, writes the hidden
// VM, writes the DB instance and creates the task, so a crash cannot leave a
// reservation nothing owns.
func (s *Service) Create(ctx context.Context, in CreateInput) (DBInstance, error) {
	if in.Engine == "" {
		in.Engine = EnginePostgres
	}
	if in.StorageGB == 0 {
		in.StorageGB = MinStorageGB
	}
	if fields := validateCreate(in); len(fields) > 0 {
		return DBInstance{}, &service.ValidationError{
			Message: "the DB instance request is not acceptable", Fields: fields,
		}
	}

	image, err := s.resolveEngineImage(ctx, in.Engine, in.EngineVersion)
	if err != nil {
		return DBInstance{}, err
	}
	// The engine version of the row is the resolved one, so a DB instance
	// created without naming a version still records which one it got.
	in.EngineVersion = image.EngineVersion.String

	// The design's floor on the data disk: at least the engine image's
	// min_disk_gb. It couples the data disk to the size of the root image,
	// which is a conservative reading of section 3 rather than a necessary
	// one, and it never refuses a size a user would reasonably ask for.
	if minimum := int(image.MinDiskGb); in.StorageGB < minimum {
		return DBInstance{}, &service.ValidationError{
			Message: "the DB instance request is not acceptable",
			Fields: []service.FieldError{{
				Field:  "storage_gb",
				Reason: fmt.Sprintf("the %s image needs at least %d GiB", in.Engine, minimum),
			}},
		}
	}

	// The verifier is computed here and the password goes no further. From
	// this line on there is no way back to it, which is why there is no
	// password reset in this version (section 5).
	verifier, err := dbcred.PostgresVerifier(in.AdminPassword)
	if err != nil {
		return DBInstance{}, &service.ValidationError{
			Message: "the DB instance request is not acceptable",
			Fields:  []service.FieldError{{Field: "admin_password", Reason: passwordReason(err)}},
		}
	}

	// The id is minted before the transaction because the stash is keyed by
	// it and the transaction may be retried: a fresh id per attempt would
	// leave a verifier behind under a key nothing reads. The stash is filled
	// before the task exists, so a worker that claims it the instant it is
	// committed finds the credential already there.
	dbInstanceID := id.DBInstance()
	s.credentials.put(dbInstanceID, verifier)

	var (
		created sqlc.DbInstance
		taskID  string
		server  instance.ManagedReserved
	)
	err = s.instances.WithManagedReservation(ctx, instance.ManagedReservation{
		Name:         in.Identifier,
		Owner:        instance.ManagedByDatabases,
		ImageID:      image.ID,
		InstanceType: in.InstanceType,
		SubnetID:     in.SubnetID,
		RootDiskGB:   int(image.MinDiskGb),
		ExtraDiskGB:  in.StorageGB,
		CreatedBy:    in.CreatedBy,
	}, func(ctx context.Context, tx *store.Tx, reserved instance.ManagedReserved) error {
		server = reserved

		row, err := tx.CreateDBInstance(ctx, sqlc.CreateDBInstanceParams{
			ID:            dbInstanceID,
			Identifier:    in.Identifier,
			State:         string(StateCreating),
			Engine:        string(in.Engine),
			EngineVersion: in.EngineVersion,
			ImageID:       image.ID,
			InstanceID:    reserved.InstanceID,
			InstanceType:  in.InstanceType,
			StorageGb:     int64(in.StorageGB),
			Port:          DefaultPort,
			AdminUsername: in.AdminUsername,
			DatabaseName:  in.DatabaseName,
			CreatedBy:     nullableString(in.CreatedBy),
			CreatedAt:     formatTime(s.now()),
		})
		if err != nil {
			if isIdentifierTaken(err) {
				return fmt.Errorf("%w: a DB instance named %q already exists",
					service.ErrAlreadyExists, in.Identifier)
			}
			return fmt.Errorf("create the DB instance row: %w", err)
		}
		created = row

		queued, err := s.tasks.CreateInTx(ctx, tx, task.TypeDBInstanceCreate, row.ID,
			createPayload{DBInstanceID: row.ID})
		if err != nil {
			return err
		}
		taskID = queued.ID
		return nil
	})
	if err != nil {
		// Nothing was committed, so the verifier has no row to belong to.
		s.credentials.forget(dbInstanceID)
		return DBInstance{}, err
	}

	out := fromRow(created)
	out.VCPUs = server.VCPUs
	out.MemoryMiB = server.MemoryMiB
	out.TaskID = &taskID

	s.logger.InfoContext(ctx, "dbinstance: create queued",
		"db_instance_id", out.ID, "identifier", out.Identifier,
		"engine", out.Engine, "engine_version", out.EngineVersion,
		"instance_id", out.InstanceID, "task_id", taskID)

	return out, nil
}

// resolveEngineImage picks the image a create boots from: the newest available
// one for the engine and version.
//
// A node with no engine image is the normal state of a fresh install, not a
// fault: it answers ErrNoEngineImage, which the console turns into an
// explanation of how to import one (section 2).
func (s *Service) resolveEngineImage(
	ctx context.Context, engine Engine, version string,
) (sqlc.Image, error) {
	if version == "" {
		available, err := s.store.ListAvailableEngineImages(ctx)
		if err != nil {
			return sqlc.Image{}, fmt.Errorf("list the engine images: %w", err)
		}
		// The listing is newest version first, so the first row for this
		// engine is the one a create without a version gets.
		for _, row := range available {
			if row.Engine.Valid && row.Engine.String == string(engine) && row.EngineVersion.Valid {
				version = row.EngineVersion.String
				break
			}
		}
		if version == "" {
			return sqlc.Image{}, fmt.Errorf("%w (engine %s)", ErrNoEngineImage, engine)
		}
	}

	row, err := s.store.GetAvailableEngineImage(ctx, sqlc.GetAvailableEngineImageParams{
		Engine:        nullableString(string(engine)),
		EngineVersion: nullableString(version),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sqlc.Image{}, fmt.Errorf("%w (engine %s %s)", ErrNoEngineImage, engine, version)
		}
		return sqlc.Image{}, fmt.Errorf("read the engine image of %s %s: %w", engine, version, err)
	}
	return row, nil
}

// createPayload is the task input of a create. It only names the DB instance:
// every other input lives on the row, so SQLite stays the single source of
// truth and a retry cannot act on stale task data. The verifier is not here
// either, and that is the point -- a task row is persisted (section 5).
type createPayload struct {
	DBInstanceID string `json:"db_instance_id"`
}

// validateCreate checks the request itself, before anything is read.
func validateCreate(in CreateInput) []service.FieldError {
	var fields []service.FieldError

	switch {
	case strings.TrimSpace(in.Identifier) == "":
		fields = append(fields, service.FieldError{Field: "identifier", Reason: "is required"})
	case len(in.Identifier) > MaxIdentifierLength:
		fields = append(fields, service.FieldError{
			Field:  "identifier",
			Reason: fmt.Sprintf("must be at most %d characters", MaxIdentifierLength),
		})
	case !identifierPattern.MatchString(in.Identifier),
		strings.HasSuffix(in.Identifier, "-"),
		strings.Contains(in.Identifier, "--"):
		fields = append(fields, service.FieldError{
			Field: "identifier",
			Reason: "must start with a letter and hold lowercase letters, digits and " +
				"single dashes, and cannot end with a dash",
		})
	}

	if in.Engine != EnginePostgres {
		fields = append(fields, service.FieldError{
			Field:  "engine",
			Reason: fmt.Sprintf("must be %q in this release", EnginePostgres),
		})
	}
	if in.EngineVersion != "" && !versionPattern.MatchString(in.EngineVersion) {
		fields = append(fields, service.FieldError{
			Field:  "engine_version",
			Reason: "must be a major version, such as \"18\"",
		})
	}
	if strings.TrimSpace(in.InstanceType) == "" {
		fields = append(fields, service.FieldError{Field: "instance_type", Reason: "is required"})
	}
	if in.SubnetID != "" && !id.HasResourceShape(id.PrefixSubnet, in.SubnetID) {
		fields = append(fields, service.FieldError{Field: "subnet_id", Reason: "must be a subnet-… id"})
	}
	if in.StorageGB < MinStorageGB || in.StorageGB > MaxStorageGB {
		fields = append(fields, service.FieldError{
			Field:  "storage_gb",
			Reason: fmt.Sprintf("must be between %d and %d GiB", MinStorageGB, MaxStorageGB),
		})
	}

	fields = append(fields, validatePostgresName("admin_username", in.AdminUsername)...)
	fields = append(fields, validatePostgresName("database_name", in.DatabaseName)...)

	// The password is validated by the package that derives from it, so the
	// rule holds even if this layer forgets it. The verifier is computed after
	// the whole request is accepted; the check is here so a short password is
	// one field error among the others rather than a second round trip.
	if err := dbcred.ValidatePassword(in.AdminPassword); err != nil {
		fields = append(fields, service.FieldError{
			Field:  "admin_password",
			Reason: passwordReason(err),
		})
	}

	return fields
}

// validatePostgresName checks the admin role and the initial database, which
// are both unquoted Postgres identifiers.
func validatePostgresName(field, name string) []service.FieldError {
	switch {
	case strings.TrimSpace(name) == "":
		return []service.FieldError{{Field: field, Reason: "is required"}}
	case len(name) > MaxNameLength:
		return []service.FieldError{{
			Field:  field,
			Reason: fmt.Sprintf("must be at most %d characters", MaxNameLength),
		}}
	case !namePattern.MatchString(name):
		return []service.FieldError{{
			Field: field,
			Reason: "must start with a lowercase letter or underscore and hold only " +
				"lowercase letters, digits and underscores",
		}}
	}
	if _, reserved := reservedNames[name]; reserved || strings.HasPrefix(name, "pg_") {
		return []service.FieldError{{
			Field:  field,
			Reason: fmt.Sprintf("%q is reserved by Postgres or by Kumo", name),
		}}
	}
	return nil
}

// passwordReason turns a dbcred refusal into the reason a form field shows. The
// password itself is never echoed.
func passwordReason(err error) string {
	switch {
	case errors.Is(err, dbcred.ErrPasswordTooShort):
		return fmt.Sprintf("must be at least %d characters", dbcred.MinPasswordLength)
	case errors.Is(err, dbcred.ErrPasswordTooLong):
		return fmt.Sprintf("must be at most %d characters", dbcred.MaxPasswordLength)
	case errors.Is(err, dbcred.ErrPasswordNotPrintableASCII):
		// Not a style rule: SCRAM normalizes a password before deriving from
		// it and Kumo does not, so anything else would authenticate nowhere
		// (docs/design/07-databases.md section 3).
		return "must hold printable ASCII only, because the engine derives its " +
			"credential from the password before Kumo can normalize it"
	default:
		return "is not acceptable"
	}
}

func nullableString(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}

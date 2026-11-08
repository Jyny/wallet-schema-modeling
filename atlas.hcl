# https://github.com/Jyny/atlas-lab/releases/tag/0.4.0 (postgres only)
# https://atlasgo.io/atlas-schema/projects

# settings come from the environment (.env via the Makefile, see .env.example)
# and fall back to the defaults below, so plain `atlas ... --env local` works too.

variable "db_user" {
  type    = string
  default = getenv("DB_USER") != "" ? getenv("DB_USER") : "user"
}

variable "db_password" {
  type    = string
  default = getenv("DB_PASSWORD") != "" ? getenv("DB_PASSWORD") : "passwd"
}

variable "db_name" {
  type    = string
  default = getenv("DB_NAME") != "" ? getenv("DB_NAME") : "default"
}

locals {
  # https://atlasgo.io/concepts/dev-database
  # postgres 18 is required: the schema uses the built-in uuidv7()
  dev_url = "docker://postgres/18/dev?search_path=public"

  # target database for local development, matching docker-compose.yaml
  local_url = "postgres://${var.db_user}:${var.db_password}@localhost:5432/${var.db_name}?search_path=public&sslmode=disable"
}

variable "schema_src" {
  # declarative mode: desired state ("schema as code")
  type    = string
  default = "file://schema/schema.sql"
}

variable "migration_dir" {
  # versioned mode: migration directory
  type    = string
  default = "file://migration"
}

variable "staging_url" {
  # target database for a deployment other than local. kept out of the file so
  # credentials are not committed: set it in .env, or
  # `STAGING_URL=... make migrate.status ATLAS_ENV=staging`
  type    = string
  default = getenv("STAGING_URL")
}

variable "revisions_schema" {
  # keep the revisions table out of the target schema ("public"), otherwise
  # declarative `schema apply` sees it as drift and plans to drop it
  type    = string
  default = "atlas"
}

variable "sql_indent" {
  # https://atlasgo.io/atlas-schema/projects#format
  type    = string
  default = "{{ sql . \"  \" }}"
}

# atlas schema inspect --env schema
# reads schema/schema.sql and prints it back normalized
env "schema" {
  dev = local.dev_url
  url = var.schema_src

  format {
    schema {
      inspect = var.sql_indent
    }
  }
}

# atlas schema inspect --env migrate
# replays the migration directory and prints the resulting schema
env "migrate" {
  dev = local.dev_url
  url = var.migration_dir

  format {
    schema {
      inspect = var.sql_indent
    }
  }
}

# main working env for both declarative and versioned commands
env "local" {
  dev = local.dev_url

  # desired state: --to for `schema apply` / `migrate diff`, --from for `schema diff`
  # https://atlasgo.io/atlas-schema/projects#schema
  schema {
    src = var.schema_src
  }

  # target database: --url for `schema apply` / `migrate apply`
  url = local.local_url

  # https://atlasgo.io/versioned/intro
  migration {
    dir              = var.migration_dir
    revisions_schema = var.revisions_schema
  }

  # https://atlasgo.io/lint/analyzers
  # the scope to lint (--latest N vs --git-base) is a per-invocation choice and
  # lives in the Makefile as ATLAS_LINT_* instead.

  format {
    schema {
      inspect = var.sql_indent
      diff    = var.sql_indent
    }
    migrate {
      diff = var.sql_indent
    }
  }
}

# deployment target for the versioned commands, e.g.
# `make migrate.status ATLAS_ENV=staging` with STAGING_URL set in .env.
# no `schema` block: applying a declarative desired state straight to a deployed
# database is exactly what versioned mode exists to avoid.
env "staging" {
  dev = local.dev_url
  url = var.staging_url

  migration {
    dir              = var.migration_dir
    revisions_schema = var.revisions_schema
  }
}

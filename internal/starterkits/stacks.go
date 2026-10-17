package starterkits

import (
	"fmt"
	"regexp"
	"strings"
)

// Every Dockerfile follows the preview runtime: the app listens on 0.0.0.0,
// writes only to /tmp (the rest of the file system is read-only) and runs as
// an unprivileged user. Comments say what to change, in the user's words.

func cmd(argv ...string) func(Choices) []string { return func(Choices) []string { return argv } }

func lines(l ...string) string { return strings.Join(l, "\n") + "\n" }

var catalog = []Stack{
	{
		ID: "nextjs", Name: "Next.js", Kind: "Full-stack", Language: "Node.js · React",
		Port: 3000, Health: "/",
		Setup: []Preset{
			{Key: "prisma", Label: "Prisma: migrate deploy", Command: cmd("node_modules/.bin/prisma", "migrate", "deploy")},
			{Key: "drizzle", Label: "Drizzle: migrate", Command: cmd("node_modules/.bin/drizzle-kit", "migrate")},
			{Key: "npm", Label: "npm run migrate", Command: cmd("npm", "run", "migrate")},
		},
		Seed: []Preset{
			{Key: "prisma", Label: "Prisma: db seed", Command: cmd("node_modules/.bin/prisma", "db", "seed")},
			{Key: "npm", Label: "npm run seed", Command: cmd("npm", "run", "seed")},
		},
		Workers:    []Preset{{Key: "npm", Label: "npm run worker", Name: "worker", Command: cmd("npm", "run", "worker")}},
		dockerfile: nextDockerfile,
		notes: func(Choices) []string {
			return []string{"Values the browser needs (NEXT_PUBLIC_…) are built into the bundle, so keep them in a committed .env.production. Runtime settings go in .previewrc.yaml or on the Secrets tab."}
		},
	},
	{
		ID: "vite", Name: "React + Vite", Kind: "Frontend", Language: "Static site",
		Port: 8080, Health: "/", Static: true,
		dockerfile: viteDockerfile,
		notes: func(Choices) []string {
			return []string{
				"The build output goes to dist/, Vite's default. Another folder? Change it in the COPY line.",
				"Values the browser needs (VITE_…) are built in at build time, so keep them in a committed .env.production.",
				"Calling an API? Use its public address: a preview can't reach your laptop's localhost.",
			}
		},
	},
	{
		ID: "node-api", Name: "Node.js API", Kind: "Backend API", Language: "Express · Fastify · NestJS",
		Port: 3000, Docs: "/docs",
		Setup: []Preset{
			{Key: "prisma", Label: "Prisma: migrate deploy", Command: cmd("node_modules/.bin/prisma", "migrate", "deploy")},
			{Key: "knex", Label: "Knex: migrate:latest", Command: cmd("node_modules/.bin/knex", "migrate:latest")},
			{Key: "npm", Label: "npm run migrate", Command: cmd("npm", "run", "migrate")},
		},
		Seed: []Preset{
			{Key: "prisma", Label: "Prisma: db seed", Command: cmd("node_modules/.bin/prisma", "db", "seed")},
			{Key: "knex", Label: "Knex: seed:run", Command: cmd("node_modules/.bin/knex", "seed:run")},
			{Key: "npm", Label: "npm run seed", Command: cmd("npm", "run", "seed")},
		},
		Workers:    []Preset{{Key: "npm", Label: "npm run worker", Name: "worker", Command: cmd("npm", "run", "worker")}},
		dockerfile: nodeAPIDockerfile,
		notes: func(c Choices) []string {
			return []string{fmt.Sprintf("Listen on 0.0.0.0 and the PORT variable (%d), not localhost.", c.Port)}
		},
	},
	{
		ID: "django", Name: "Django", Kind: "Full-stack", Language: "Python",
		Port: 8000, Docs: "/api/docs/",
		Detail: &Detail{Label: "Project package", Hint: "The folder that holds settings.py and wsgi.py, such as mysite.", Default: "mysite",
			Pattern: regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)},
		Setup: []Preset{
			{Key: "migrate", Label: "manage.py migrate", Command: cmd("python", "manage.py", "migrate", "--noinput")},
		},
		Seed: []Preset{
			{Key: "loaddata", Label: "manage.py loaddata preview", Command: cmd("python", "manage.py", "loaddata", "preview")},
		},
		Workers: []Preset{{Key: "celery", Label: "Celery worker", Name: "celery", Command: func(c Choices) []string {
			return []string{"celery", "-A", c.Detail, "worker", "--loglevel=info"}
		}}},
		dockerfile: djangoDockerfile,
		notes: func(c Choices) []string {
			n := []string{"Allow your preview's address: add it to ALLOWED_HOSTS and CSRF_TRUSTED_ORIGINS, for example from environment variables you set under env."}
			if c.Postgres != "" {
				n = append(n, "Add psycopg[binary] and dj-database-url to requirements.txt, and read DATABASE_URL in settings.py.")
			}
			return n
		},
	},
	{
		ID: "fastapi", Name: "FastAPI", Kind: "Backend API", Language: "Python",
		Port: 8000, Docs: "/docs", DocsOn: true,
		Detail: &Detail{Label: "App module", Hint: "Where your FastAPI app lives, as module:variable, such as app.main:app.", Default: "app.main:app",
			Pattern: regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]{0,99}:[A-Za-z_][A-Za-z0-9_]{0,63}$`)},
		Setup: []Preset{
			{Key: "alembic", Label: "Alembic: upgrade head", Command: cmd("alembic", "upgrade", "head")},
		},
		Seed: []Preset{
			{Key: "module", Label: "python -m app.seed", Command: cmd("python", "-m", "app.seed")},
		},
		Workers: []Preset{
			{Key: "celery", Label: "Celery worker (app.worker)", Name: "celery", Command: cmd("celery", "-A", "app.worker", "worker", "--loglevel=info")},
			{Key: "arq", Label: "arq worker (app.worker.WorkerSettings)", Name: "arq", Command: cmd("arq", "app.worker.WorkerSettings")},
		},
		dockerfile: fastapiDockerfile,
		notes: func(c Choices) []string {
			if c.Postgres == "" {
				return nil
			}
			return []string{"SQLAlchemy needs postgresql:// (or postgresql+psycopg://): replace the postgres:// at the start of DATABASE_URL when you read it."}
		},
	},
	{
		ID: "rails", Name: "Ruby on Rails", Kind: "Full-stack", Language: "Ruby",
		Port: 3000, Health: "/up",
		Setup: []Preset{
			{Key: "migrate", Label: "rails db:migrate", Command: cmd("bin/rails", "db:migrate")},
			{Key: "prepare", Label: "rails db:prepare (migrates, and seeds a new database)", Command: cmd("bin/rails", "db:prepare")},
		},
		Seed: []Preset{{Key: "seed", Label: "rails db:seed", Command: cmd("bin/rails", "db:seed")}},
		Workers: []Preset{
			{Key: "sidekiq", Label: "Sidekiq", Name: "sidekiq", Command: cmd("bundle", "exec", "sidekiq")},
			{Key: "solid", Label: "Solid Queue (bin/jobs)", Name: "jobs", Command: cmd("bin/jobs")},
		},
		dockerfile: railsDockerfile,
		notes: func(c Choices) []string {
			n := []string{
				"Add SECRET_KEY_BASE on the repository's Secrets tab (bin/rails secret makes one), or RAILS_MASTER_KEY.",
				"Using jsbundling or cssbundling? Install Node.js in the image before assets:precompile.",
			}
			if c.Postgres == "" && c.MySQL == "" {
				n = append(n, "Rails 8 uses SQLite by default, but previews keep no files outside /tmp: turn on PostgreSQL and the pg gem for a database.")
			}
			return n
		},
	},
	{
		ID: "go", Name: "Go", Kind: "Backend API", Language: "net/http · Gin · Echo",
		Port: 8080, Docs: "/swagger/", NoShell: true,
		Detail: &Detail{Label: "Main package", Hint: "The package with func main, as a path from the repository root, such as ./cmd/server.", Default: ".",
			Pattern: regexp.MustCompile(`^\.(/[A-Za-z0-9_-][A-Za-z0-9_.-]*){0,8}$`)},
		Setup: []Preset{
			{Key: "migrate", Label: "Your app: server migrate", Command: cmd("/app/server", "migrate")},
		},
		Seed:       []Preset{{Key: "seed", Label: "Your app: server seed", Command: cmd("/app/server", "seed")}},
		Workers:    []Preset{{Key: "worker", Label: "Your app: server worker", Name: "worker", Command: cmd("/app/server", "worker")}},
		dockerfile: goDockerfile,
		notes: func(c Choices) []string {
			n := []string{fmt.Sprintf("Listen on 0.0.0.0:%d, or read the PORT variable.", c.Port),
				"The final image has no shell, so setup, seed and worker commands run your program directly."}
			if c.Postgres != "" {
				n = append(n, "Read DATABASE_URL as it is: pgx and lib/pq both accept it.")
			}
			return n
		},
	},
	{
		ID: "spring", Name: "Spring Boot", Kind: "Backend API", Language: "Java",
		Port: 8080, Docs: "/swagger-ui/index.html",
		Detail:     &Detail{Label: "Build tool", Hint: "How the project builds.", Default: "Maven", Options: []string{"Maven", "Gradle"}},
		Wrapper:    []string{"preview-env"},
		Workers:    []Preset{{Key: "profile", Label: "This app with a worker profile", Name: "worker", Command: cmd("preview-env", "java", "-jar", "/app/app.jar", "--spring.profiles.active=worker")}},
		dockerfile: springDockerfile,
		notes: func(c Choices) []string {
			n := []string{"Flyway and Liquibase run when the app starts, so most Spring apps need no setup step. For reviewer data, add a preview-only Flyway location (or a CommandLineRunner in a preview profile) that inserts synthetic records."}
			if c.Postgres != "" {
				n = append(n, "preview-env turns Previa's PostgreSQL into SPRING_DATASOURCE_URL, username and password. Add the org.postgresql:postgresql driver.")
			}
			if c.MySQL != "" {
				n = append(n, "preview-env maps MySQL to SPRING_DATASOURCE_URL, username and password when PostgreSQL is not selected. Add the MySQL JDBC driver to your project.")
			}
			return append(n, "Using Spring Boot Actuator? Set the health check path to /actuator/health under Advanced.")
		},
	},
	{
		ID: "laravel", Name: "Laravel", Kind: "Full-stack", Language: "PHP",
		Port: 8000, Health: "/up",
		Wrapper: []string{"preview-env"},
		Setup: []Preset{
			{Key: "migrate", Label: "artisan migrate", Command: cmd("preview-env", "php", "artisan", "migrate", "--force")},
		},
		Seed:       []Preset{{Key: "seed", Label: "artisan db:seed", Command: cmd("preview-env", "php", "artisan", "db:seed", "--force")}},
		Workers:    []Preset{{Key: "queue", Label: "artisan queue:work", Name: "queue", Command: cmd("preview-env", "php", "artisan", "queue:work", "--tries=3")}},
		dockerfile: laravelDockerfile,
		notes: func(Choices) []string {
			return []string{
				"Add APP_KEY on the repository's Secrets tab (php artisan key:generate --show makes one).",
				"preview-env keeps Laravel's storage in /tmp and passes Previa's DATABASE_URL on as DB_URL.",
			}
		},
	},
	{
		ID: "custom", Name: "Own Dockerfile", Kind: "Any stack", Language: "You already have one",
		Port: 3000, OwnFile: true, Docs: "/docs",
		Detail: &Detail{Label: "Dockerfile path", Hint: "A path inside the repository, such as Dockerfile or docker/web.Dockerfile.", Default: "Dockerfile",
			Pattern: regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*){0,8}$`)},
		notes: func(c Choices) []string {
			return []string{
				fmt.Sprintf("Your app must listen on 0.0.0.0:%d.", c.Port),
				"Previews have a read-only file system: write only to /tmp. An image that runs as root runs as an unprivileged user.",
			}
		},
	},
}

func nextDockerfile(c Choices) string {
	return lines(
		"# Dockerfile for Next.js, from Previa's starter kits.",
		"FROM node:24-alpine",
		"WORKDIR /app",
		"",
		"# Dependencies first, so they're cached between builds.",
		"# Using pnpm or Yarn? Copy its lockfile and use its install command.",
		"COPY package.json package-lock.json* ./",
		"RUN npm ci",
		"",
		"COPY . .",
		"ENV NEXT_TELEMETRY_DISABLED=1",
		"RUN npm run build",
		"",
		"# Previews have a read-only file system with a writable /tmp,",
		"# so Next.js keeps its cache there.",
		"RUN rm -rf .next/cache && ln -s /tmp/next-cache .next/cache",
		"",
		"ENV NODE_ENV=production",
		fmt.Sprintf("EXPOSE %d", c.Port),
		fmt.Sprintf(`CMD ["sh", "-c", "mkdir -p /tmp/next-cache && exec node_modules/.bin/next start -H 0.0.0.0 -p %d"]`, c.Port),
	)
}

func viteDockerfile(c Choices) string {
	return lines(
		"# syntax=docker/dockerfile:1",
		"# Dockerfile for React + Vite, from Previa's starter kits.",
		"FROM node:24-alpine AS build",
		"WORKDIR /app",
		"COPY package.json package-lock.json* ./",
		"RUN npm ci",
		"COPY . .",
		"RUN npm run build",
		"",
		"# Serve the built site with nginx, as an unprivileged user.",
		"FROM nginxinc/nginx-unprivileged:stable-alpine",
		"COPY --from=build /app/dist /usr/share/nginx/html",
		"# Single-page app: unknown paths load index.html.",
		"COPY <<'EOF' /etc/nginx/conf.d/default.conf",
		"server {",
		fmt.Sprintf("  listen %d;", c.Port),
		"  root /usr/share/nginx/html;",
		"  location / {",
		"    try_files $uri /index.html;",
		"  }",
		"}",
		"EOF",
		fmt.Sprintf("EXPOSE %d", c.Port),
	)
}

func nodeAPIDockerfile(c Choices) string {
	return lines(
		"# Dockerfile for a Node.js API, from Previa's starter kits.",
		"FROM node:24-alpine",
		"WORKDIR /app",
		"",
		"# Dependencies first, so they're cached between builds.",
		"COPY package.json package-lock.json* ./",
		"RUN npm ci",
		"",
		"COPY . .",
		"# Builds TypeScript when package.json has a build script.",
		"RUN npm run build --if-present",
		"",
		fmt.Sprintf("ENV NODE_ENV=production PORT=%d", c.Port),
		fmt.Sprintf("EXPOSE %d", c.Port),
		`CMD ["npm", "start"]`,
	)
}

func djangoDockerfile(c Choices) string {
	return lines(
		"# Dockerfile for Django, from Previa's starter kits.",
		"FROM python:3.13-slim",
		"ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1",
		"WORKDIR /app",
		"",
		"COPY requirements.txt .",
		"RUN pip install --no-cache-dir -r requirements.txt gunicorn",
		"",
		"COPY . .",
		"# Serving static files with WhiteNoise? Collect them into the image:",
		"# RUN python manage.py collectstatic --noinput",
		"",
		fmt.Sprintf("EXPOSE %d", c.Port),
		fmt.Sprintf(`CMD ["gunicorn", "%s.wsgi:application", "--bind", "0.0.0.0:%d", "--workers", "2"]`, c.Detail, c.Port),
	)
}

func fastapiDockerfile(c Choices) string {
	return lines(
		"# Dockerfile for FastAPI, from Previa's starter kits.",
		"FROM python:3.13-slim",
		"ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1",
		"WORKDIR /app",
		"",
		"# requirements.txt should include fastapi and uvicorn.",
		"COPY requirements.txt .",
		"RUN pip install --no-cache-dir -r requirements.txt",
		"",
		"COPY . .",
		fmt.Sprintf("EXPOSE %d", c.Port),
		fmt.Sprintf(`CMD ["uvicorn", "%s", "--host", "0.0.0.0", "--port", "%d"]`, c.Detail, c.Port),
	)
}

func railsDockerfile(c Choices) string {
	packages := "build-essential git libyaml-dev pkg-config"
	if c.Postgres != "" {
		packages += " libpq-dev"
	}
	if c.MySQL != "" {
		packages += " default-libmysqlclient-dev"
	}
	return lines(
		"# Dockerfile for Ruby on Rails, from Previa's starter kits.",
		"FROM ruby:3.4-slim",
		"WORKDIR /rails",
		"",
		"RUN apt-get update -qq && \\",
		"    apt-get install --no-install-recommends -y \\",
		"      "+packages+" && \\",
		"    rm -rf /var/lib/apt/lists/*",
		"",
		"ENV RAILS_ENV=production RAILS_LOG_TO_STDOUT=1 \\",
		"    BUNDLE_DEPLOYMENT=1 BUNDLE_WITHOUT=development:test",
		"COPY Gemfile Gemfile.lock ./",
		"RUN bundle install",
		"",
		"COPY . .",
		"RUN SECRET_KEY_BASE_DUMMY=1 bin/rails assets:precompile",
		"",
		"# Previews have a read-only file system: send tmp/ and log/ to /tmp.",
		"RUN rm -rf tmp log && ln -s /tmp tmp && ln -s /tmp log",
		"",
		fmt.Sprintf("EXPOSE %d", c.Port),
		fmt.Sprintf(`CMD ["bin/rails", "server", "-b", "0.0.0.0", "-p", "%d"]`, c.Port),
	)
}

func goDockerfile(c Choices) string {
	return lines(
		"# Dockerfile for Go, from Previa's starter kits.",
		"FROM golang:1.26-alpine AS build",
		"WORKDIR /src",
		"",
		"# Modules first, so they're cached between builds.",
		"COPY go.mod go.sum* ./",
		"RUN go mod download",
		"",
		"COPY . .",
		fmt.Sprintf("RUN CGO_ENABLED=0 go build -trimpath -o /out/server %s", c.Detail),
		"",
		"# A minimal image that runs as an unprivileged user.",
		"FROM gcr.io/distroless/static-debian12:nonroot",
		"COPY --from=build /out/server /app/server",
		fmt.Sprintf("ENV PORT=%d", c.Port),
		fmt.Sprintf("EXPOSE %d", c.Port),
		`ENTRYPOINT ["/app/server"]`,
	)
}

// previewEnv is a small entrypoint that maps Previa's service settings to
// the names a framework reads, then runs the command it is given. Setup and
// processes call it too, because they replace the image's entrypoint.
func previewEnv(body ...string) []string {
	head := []string{"COPY --chmod=755 <<'EOF' /usr/local/bin/preview-env", "#!/bin/sh"}
	return append(append(head, body...), `exec "$@"`, "EOF")
}

func springDockerfile(c Choices) string {
	build := []string{
		"FROM maven:3.9-eclipse-temurin-21 AS build",
		"WORKDIR /src",
		"COPY pom.xml .",
		"RUN mvn -q dependency:go-offline",
		"COPY src ./src",
		"RUN mvn -q package -DskipTests",
		"",
		"FROM eclipse-temurin:21-jre",
		"WORKDIR /app",
		"COPY --from=build /src/target/*.jar /app/app.jar",
	}
	if c.Detail == "Gradle" {
		build = []string{
			"FROM gradle:8-jdk21 AS build",
			"WORKDIR /src",
			"COPY . .",
			"RUN gradle bootJar --no-daemon",
			"",
			"FROM eclipse-temurin:21-jre",
			"WORKDIR /app",
			"COPY --from=build /src/build/libs/*.jar /app/app.jar",
		}
	}
	out := []string{"# syntax=docker/dockerfile:1", "# Dockerfile for Spring Boot, from Previa's starter kits."}
	out = append(out, build...)
	out = append(out, "", "# Spring reads its own variable names, so pass Previa's on.")
	out = append(out, previewEnv(
		`if [ -n "$PGHOST" ]; then`,
		`  export SPRING_DATASOURCE_URL="jdbc:postgresql://$PGHOST:$PGPORT/$PGDATABASE"`,
		`  export SPRING_DATASOURCE_USERNAME="$PGUSER"`,
		`  export SPRING_DATASOURCE_PASSWORD="$PGPASSWORD"`,
		`elif [ -n "$MYSQL_HOST" ]; then`,
		`  export SPRING_DATASOURCE_URL="jdbc:mysql://$MYSQL_HOST:$MYSQL_PORT/$MYSQL_DATABASE"`,
		`  export SPRING_DATASOURCE_USERNAME="$MYSQL_USER"`,
		`  export SPRING_DATASOURCE_PASSWORD="$MYSQL_PASSWORD"`,
		`fi`,
		`if [ -n "$REDIS_URL" ]; then`,
		`  export SPRING_DATA_REDIS_URL="$REDIS_URL"`,
		`fi`,
	)...)
	out = append(out, "",
		fmt.Sprintf("EXPOSE %d", c.Port),
		`ENTRYPOINT ["preview-env"]`,
		fmt.Sprintf(`CMD ["java", "-jar", "/app/app.jar", "--server.port=%d"]`, c.Port),
	)
	return lines(out...)
}

func laravelDockerfile(c Choices) string {
	extensions, system := "zip", "libzip-dev unzip"
	if c.Postgres != "" {
		extensions, system = "pdo_pgsql zip", "libpq-dev libzip-dev unzip"
	}
	if c.MySQL != "" {
		extensions += " pdo_mysql"
	}
	out := []string{
		"# syntax=docker/dockerfile:1",
		"# Dockerfile for Laravel, from Previa's starter kits.",
		"FROM composer:2 AS vendor",
		"WORKDIR /app",
		"COPY composer.json composer.lock ./",
		"RUN composer install --no-dev --no-scripts --no-interaction \\",
		"    --prefer-dist --ignore-platform-reqs",
		"COPY . .",
		"RUN composer dump-autoload --optimize --no-dev",
		"",
		"FROM php:8.4-cli",
		"RUN apt-get update && \\",
		"    apt-get install -y --no-install-recommends \\",
		"      " + system + " && \\",
		"    docker-php-ext-install " + extensions + " && \\",
		"    rm -rf /var/lib/apt/lists/*",
	}
	if c.Redis != "" {
		out = append(out, "RUN pecl install redis && docker-php-ext-enable redis")
	}
	out = append(out,
		"WORKDIR /app",
		"COPY --from=vendor /app /app",
		"",
		"# Previews have a read-only file system: keep storage in /tmp,",
		"# and pass Previa's database address on as Laravel's DB_URL.",
		"ENV LARAVEL_STORAGE_PATH=/tmp/storage \\",
		"    LOG_CHANNEL=stderr PHP_CLI_SERVER_WORKERS=4",
	)
	out = append(out, previewEnv(
		`mkdir -p /tmp/storage/logs /tmp/storage/framework/cache`,
		`mkdir -p /tmp/storage/framework/sessions /tmp/storage/framework/views`,
		`if [ -n "$DATABASE_URL" ]; then`,
		`  export DB_URL="$DATABASE_URL"`,
		`  if [ -n "$PGHOST" ]; then`,
		`    export DB_CONNECTION=pgsql`,
		`  elif [ -n "$MYSQL_HOST" ]; then`,
		`    export DB_CONNECTION=mysql`,
		`  else`,
		`    export DB_CONNECTION=pgsql`,
		`  fi`,
		`fi`,
	)...)
	out = append(out, "",
		fmt.Sprintf("EXPOSE %d", c.Port),
		`ENTRYPOINT ["preview-env"]`,
		fmt.Sprintf(`CMD ["php", "artisan", "serve", "--host=0.0.0.0", "--port=%d"]`, c.Port),
	)
	return lines(out...)
}

// Package opensource renders the public Open Source Tests: well-known
// open-source apps Previa is tested against, chosen to exercise it across
// stacks, sizes and configurations. Each carries the .previewrc.yaml used, the
// steps and the results that must hold, so the page is both the published test
// method and, as results come in, the record of what has been verified. It is
// static copy and reads no saved state.
//
// In text fields, `backticks` mark code; see inline in stacks.templ.
package opensource

// Repo is one app under test. Its anchor is #repo-<ID>.
type Repo struct {
	ID   string
	Name string // owner/name on GitHub
	URL  string
	// Short labels the repository in compact lists when its GitHub name alone
	// is unclear or long, such as miniflux/v2.
	Short string
	// Core apps are the first ten tested; between them they cover
	// most of Capabilities across TypeScript, Go, Python, Ruby, Java and PHP.
	Core  bool
	Kind  string // Frontend, Backend API or Full-stack
	Size  string // Small, Medium or Large
	Level string // configuration effort: Basic, Intermediate or Advanced
	Stack []string
	// Architecture says how the app is put together, in one sentence.
	Architecture string
	// Why says what this repository proves about Previa.
	Why    string
	Covers []string // Capability IDs
	// Config is the .previewrc.yaml used. It has not passed until Status says so.
	Config  string
	Secrets []Secret
	Changes []string // edits made to the project before its first build
	Steps   []string
	Expect  []string
	Watch   []string // known risks and limits worth recording
	Status  Status
}

// Secret is a value to add on the repository's Secrets tab.
type Secret struct {
	Name, Hint string
}

// Status records verification. Set Verified (YYYY-MM-DD) only after every
// Expect has held on a real preview; until then the app shows "In testing".
type Status struct {
	Verified string
	Note     string
}

// Capability is something Previa does that an app's test exercises.
type Capability struct {
	ID, Name, Group string
}

// CapabilityGroups lists the coverage groups in reading order.
func CapabilityGroups() []string { return []string{"Build", "Run", "Review and lifecycle"} }

// Capabilities lists what the apps exercise, grouped and in reading order.
func Capabilities() []Capability {
	return []Capability{
		{ID: "static", Name: "Static sites", Group: "Build"},
		{ID: "monorepo", Name: "Monorepos", Group: "Build"},
		{ID: "path", Name: "Dockerfile in a subfolder", Group: "Build"},
		{ID: "context", Name: "Build context folder", Group: "Build"},
		{ID: "kits", Name: "Starter kits", Group: "Build"},
		{ID: "limits", Name: "Heavy builds and limits", Group: "Build"},
		{ID: "diagnosis", Name: "Submodule and LFS checks", Group: "Build"},

		{ID: "ssr", Name: "Server-rendered apps", Group: "Run"},
		{ID: "api", Name: "APIs with a docs link", Group: "Run"},
		{ID: "postgres", Name: "PostgreSQL", Group: "Run"},
		{ID: "mysql", Name: "MySQL", Group: "Run"},
		{ID: "redis", Name: "Redis", Group: "Run"},
		{ID: "setup", Name: "Setup command", Group: "Run"},
		{ID: "workers", Name: "Background processes", Group: "Run"},
		{ID: "secrets", Name: "Secrets", Group: "Run"},
		{ID: "websockets", Name: "WebSockets", Group: "Run"},
		{ID: "internet", Name: "Calls the public web", Group: "Run"},
		{ID: "outbound", Name: "Approved outbound connection", Group: "Run"},
		{ID: "readonly", Name: "Read-only file system", Group: "Run"},

		{ID: "rollback", Name: "Failed build keeps serving", Group: "Review and lifecycle"},
		{ID: "cleanup", Name: "Close, reopen, same link", Group: "Review and lifecycle"},
		{ID: "automatic", Name: "Automatic builds", Group: "Review and lifecycle"},
		{ID: "forks", Name: "Fork pull requests", Group: "Review and lifecycle"},
		{ID: "private", Name: "Private previews", Group: "Review and lifecycle"},
		{ID: "conflicts", Name: "Merge conflicts", Group: "Review and lifecycle"},
	}
}

// InGroup returns the capabilities of one group, in reading order.
func InGroup(group string) []Capability {
	var caps []Capability
	for _, c := range Capabilities() {
		if c.Group == group {
			caps = append(caps, c)
		}
	}
	return caps
}

// CapabilityName returns a capability's display name.
func CapabilityName(id string) string {
	for _, c := range Capabilities() {
		if c.ID == id {
			return c.Name
		}
	}
	return id
}

// Covering returns the repositories whose tests exercise a capability, core
// repositories first.
func Covering(id string) []Repo {
	var core, more []Repo
	for _, r := range Repos() {
		for _, c := range r.Covers {
			if c != id {
				continue
			}
			if r.Core {
				core = append(core, r)
			} else {
				more = append(more, r)
			}
			break
		}
	}
	return append(core, more...)
}

// Core returns the first set to test; More returns the rest.
func Core() []Repo { return filter(true) }

// More returns the repositories outside the core set.
func More() []Repo { return filter(false) }

func filter(core bool) []Repo {
	var repos []Repo
	for _, r := range Repos() {
		if r.Core == core {
			repos = append(repos, r)
		}
	}
	return repos
}

// VerifiedCount is how many apps have passed every check.
func VerifiedCount() int {
	n := 0
	for _, r := range Repos() {
		if r.Status.Verified != "" {
			n++
		}
	}
	return n
}

// Repos lists every repository in reading order: the core set, then the rest.
func Repos() []Repo {
	return []Repo{
		{
			ID: "mermaid-live-editor", Name: "mermaid-js/mermaid-live-editor", URL: "https://github.com/mermaid-js/mermaid-live-editor",
			Core: true, Kind: "Frontend", Size: "Medium", Level: "Basic",
			Stack:        []string{"SvelteKit", "TypeScript", "pnpm", "Static build", "nginx"},
			Architecture: "A single-page app built to static files in a multi-stage Dockerfile and served by nginx.",
			Why:          "The quickest proof of the core loop: pull request, build, private link, living comment.",
			Covers:       []string{"static", "readonly", "rollback", "cleanup"},
			Config: `port: 8080
health_check_path: /
resource_tier: small
`,
			Changes: []string{
				"None for the first build: run it unchanged to see how Previa reports the stock nginx image on a read-only file system.",
				"Then change the last `FROM` line to `nginxinc/nginx-unprivileged:stable-alpine`; the repository's nginx.conf already listens on 8080.",
			},
			Steps: []string{
				"Add `.previewrc.yaml`, open a pull request in the test copy and press Build latest commit.",
				"If the stock nginx stage fails, read the explanation, switch to the unprivileged image, push and build again.",
				"Open the link and draw a flowchart; the URL updates as you type.",
				"Push a commit that breaks the build, such as a syntax error in a `.svelte` file, and build it.",
				"Close the pull request, reopen it and build again.",
			},
			Expect: []string{
				"The pull request comment moves from Queued to Building to Ready, with a check on the exact commit.",
				"The editor renders diagrams at the private HTTPS link.",
				"The broken commit fails with a plain-language reason while the previous version keeps serving at the same link.",
				"Closing removes the preview at once; after reopening, a new build returns at the same link.",
			},
			Watch: []string{"A first build downloads every dependency; note its duration to compare with later builds."},
		},
		{
			ID: "umami", Name: "umami-software/umami", URL: "https://github.com/umami-software/umami",
			Core: true, Kind: "Full-stack", Size: "Large", Level: "Intermediate",
			Stack:        []string{"Next.js", "React", "TypeScript", "Prisma", "PostgreSQL", "pnpm"},
			Architecture: "A Next.js standalone server with API routes; Prisma migrations run each time the container starts.",
			Why:          "A widely used analytics product: a heavy Next.js build, a managed database and migrations on boot.",
			Covers:       []string{"ssr", "postgres", "secrets", "forks", "private"},
			Config: `port: 3000
health_check_path: /api/heartbeat
services:
  postgres: 17
`,
			Secrets: []Secret{{Name: "APP_SECRET", Hint: "any long random string"}},
			Steps: []string{
				"Add the config and `APP_SECRET`, open a pull request and build.",
				"Sign in with Umami's default `admin` / `umami` account, add a website and open its tracking code.",
				"From a second GitHub account, fork the test copy, open a pull request against it and build it from Previa.",
				"Turn on Private previews for the repository and open the link in a private window.",
			},
			Expect: []string{
				"The build log shows the database check and migrations; the app passes `/api/heartbeat` and the dashboard loads.",
				"The fork's pull request doesn't build on its own and its preview runs without your secrets (Umami derives a fallback key).",
				"With private previews on, visitors sign in with GitHub and only people who can see the repository get in.",
			},
			Watch: []string{"Next.js builds use a lot of memory; if the image step is stopped, record the step and the message."},
		},
		{
			ID: "miniflux", Name: "miniflux/v2", URL: "https://github.com/miniflux/v2",
			Short: "miniflux",
			Core:  true, Kind: "Full-stack", Size: "Medium", Level: "Intermediate",
			Stack:        []string{"Go", "PostgreSQL", "Server-rendered HTML", "Alpine"},
			Architecture: "One Go binary serves the UI and API; its Dockerfile lives outside the repository root.",
			Why:          "A compact Go app that proves custom Dockerfile paths, managed PostgreSQL and access to the public web.",
			Covers:       []string{"path", "ssr", "postgres", "secrets", "internet", "outbound", "automatic"},
			Config: `dockerfile: packaging/docker/alpine/Dockerfile
port: 8080
health_check_path: /healthcheck
resource_tier: small
services:
  postgres: 17
env:
  - RUN_MIGRATIONS=1
  - CREATE_ADMIN=1
  - ADMIN_USERNAME=admin
`,
			Secrets: []Secret{{Name: "ADMIN_PASSWORD", Hint: "at least 6 characters"}},
			Steps: []string{
				"Build, then sign in as `admin` with your `ADMIN_PASSWORD`.",
				"Subscribe to a public feed such as `https://github.blog/feed/`.",
				"Switch the repository to Automatic, push three small commits within two minutes and watch the Builds page.",
				"Outbound test: create a free external PostgreSQL database, remove `services`, save its connection string as the `DATABASE_URL` secret, approve its `host:5432` on the Outbound tab and build again.",
			},
			Expect: []string{
				"Migrations run on start, `/healthcheck` passes and the admin can sign in.",
				"Feed entries load, so the preview reaches the public web.",
				"Automatic mode makes one build, about five minutes after the last push.",
				"With the approval, the preview uses the external database; without it, a new preview can't reach it.",
			},
			Watch: []string{"Use a throwaway external database: anything a preview can reach is test data."},
		},
		{
			ID: "full-stack-fastapi-template", Name: "fastapi/full-stack-fastapi-template", URL: "https://github.com/fastapi/full-stack-fastapi-template",
			Short: "fastapi-template",
			Core:  true, Kind: "Full-stack", Size: "Medium", Level: "Intermediate",
			Stack:        []string{"FastAPI", "Python 3.14", "SQLModel", "Alembic", "React", "Vite", "Bun", "PostgreSQL"},
			Architecture: "A monorepo: Bun builds the React frontend, uv installs the API, and one image serves both.",
			Why:          "A modern Python monorepo with a setup step, an API docs link and BuildKit cache mounts.",
			Covers:       []string{"monorepo", "path", "api", "postgres", "setup", "secrets", "conflicts"},
			Config: `dockerfile: backend/Dockerfile
port: 8000
health_check_path: /api/v1/utils/health-check/
services:
  postgres: 17
setup: ["bash", "scripts/prestart.sh"]
env:
  - PROJECT_NAME=Previa test
  - FIRST_SUPERUSER=admin@example.com
docs_path: /docs
`,
			Secrets: []Secret{
				{Name: "SECRET_KEY", Hint: "any long random string"},
				{Name: "FIRST_SUPERUSER_PASSWORD", Hint: "anything except changethis"},
			},
			Steps: []string{
				"Build, open the link and sign in as `admin@example.com`.",
				"Open Docs from the pull request page.",
				"In a throwaway commit, change setup to `[\"sh\", \"-c\", \"echo $FIRST_SUPERUSER_PASSWORD && bash scripts/prestart.sh\"]`, build and read the log.",
				"Open two pull requests that edit the same line of `README.md`, merge one, then try to build the other.",
			},
			Expect: []string{
				"Setup output (migrations, then the first user) streams into the build log before the app starts.",
				"The Docs link opens Swagger UI at `/docs`.",
				"The password appears masked in the log, never in plain text.",
				"The conflicting pull request can't build, and the note beside the button says why.",
			},
			Watch: []string{"The template reads `DATABASE_URL` and switches it to its own driver, so no code change is needed."},
		},
		{
			ID: "bakerydemo", Name: "wagtail/bakerydemo", URL: "https://github.com/wagtail/bakerydemo",
			Core: true, Kind: "Full-stack", Size: "Medium", Level: "Advanced",
			Stack:        []string{"Django", "Wagtail CMS", "Python 3.12", "uWSGI", "PostgreSQL", "Redis"},
			Architecture: "A server-rendered CMS with an editor; an entrypoint script waits for the database before uWSGI starts.",
			Why:          "A content-heavy Django site where setup, sample data and a read-only file system meet.",
			Covers:       []string{"ssr", "postgres", "redis", "setup", "secrets", "readonly"},
			Config: `port: 8000
health_check_path: /
services:
  postgres: 17
  redis: 8
setup:
  - sh
  - -c
  - python manage.py migrate --noinput && python manage.py loaddata bakerydemo/base/fixtures/bakerydemo.json
env:
  - DJANGO_ALLOWED_HOSTS=*
`,
			Secrets: []Secret{{Name: "DJANGO_SECRET_KEY", Hint: "any long random string"}},
			Steps: []string{
				"Build and open the bakery home page, blog and breads pages.",
				"Sign in at `/admin/` with the sample data's `admin` / `changeme` account and publish an edit.",
				"Optional: change setup to the project's `load_initial_data` command and build again.",
			},
			Expect: []string{
				"Migrations and sample data load in setup; pages and the Wagtail admin work.",
				"Images show as missing placeholders: the sample photos are copied by `load_initial_data`, not by `loaddata`.",
				"`load_initial_data` fails in setup because it writes media into the read-only app folder; the log says so.",
			},
			Watch: []string{
				"The settings append `/0` and `/1` to `REDIS_URL`, which already ends in a database number; if caching errors appear, remove `redis` and the site falls back to memory.",
				"Files written in setup don't reach the app: only database changes carry over.",
			},
		},
		{
			ID: "saleor", Name: "saleor/saleor", URL: "https://github.com/saleor/saleor",
			Core: true, Kind: "Backend API", Size: "Large", Level: "Advanced",
			Stack:        []string{"Django", "GraphQL", "Python 3.12", "Uvicorn", "Celery", "PostgreSQL", "Redis"},
			Architecture: "A headless commerce API: one GraphQL endpoint, Celery for background jobs and no bundled storefront.",
			Why:          "A large production API with hundreds of migrations, a worker process and the large preview size.",
			Covers:       []string{"api", "postgres", "redis", "setup", "workers", "secrets", "limits"},
			Config: `port: 8000
health_check_path: /graphql/
resource_tier: large
services:
  postgres: 17
  redis: 8
setup:
  - sh
  - -c
  - python3 manage.py migrate --noinput && python3 manage.py createsuperuser --noinput --email admin@example.com
processes:
  - name: celery
    command: ["celery", "--app", "saleor.celeryconf:app", "worker", "--loglevel=info"]
env:
  - DEBUG=False
  - ALLOWED_HOSTS=*
  - ALLOWED_CLIENT_HOSTS=*
docs_path: /graphql/
`,
			Secrets: []Secret{
				{Name: "SECRET_KEY", Hint: "any long random string"},
				{Name: "DJANGO_SUPERUSER_PASSWORD", Hint: "password for admin@example.com"},
			},
			Changes: []string{
				"Celery reads only `CELERY_BROKER_URL`. In `saleor/settings.py`, fall back to Previa's Redis: `os.environ.get(\"CELERY_BROKER_URL\", os.environ.get(\"REDIS_URL\"))`.",
			},
			Steps: []string{
				"Make the one-line change, add the config and secrets, and build.",
				"Open Docs (the GraphQL playground) and run `{ shop { name } }`.",
				"Run `mutation { tokenCreate(email: \"admin@example.com\", password: \"…\") { token errors { message } } }`.",
			},
			Expect: []string{
				"The large build finishes within the build limits; record its duration.",
				"Setup applies every migration and creates the admin; the comment reads Runs with PostgreSQL 17 and Redis 8 · setup ran · process celery.",
				"The playground answers queries and `tokenCreate` returns a token.",
			},
			Watch: []string{"Saleor's sample data (`populatedb`) writes product images into the read-only app folder; skip it or expect it to fail."},
		},
		{
			ID: "sure", Name: "we-promise/sure", URL: "https://github.com/we-promise/sure",
			Core: true, Kind: "Full-stack", Size: "Large", Level: "Advanced",
			Stack:        []string{"Ruby on Rails", "Ruby 3.4", "Hotwire", "PostgreSQL", "Redis", "Sidekiq"},
			Architecture: "A server-rendered Rails app with Sidekiq jobs; the community-maintained successor of the Maybe finance app.",
			Why:          "A real Rails product: database preparation, a Sidekiq worker and the read-only file system rules.",
			Covers:       []string{"ssr", "postgres", "redis", "setup", "workers", "secrets", "readonly"},
			Config: `port: 3000
health_check_path: /up
resource_tier: large
services:
  postgres: 17
  redis: 8
setup: ["./bin/rails", "db:prepare"]
processes:
  - name: sidekiq
    command: ["bundle", "exec", "sidekiq"]
env:
  - SELF_HOSTED=true
  - RAILS_ASSUME_SSL=true
  - RAILS_FORCE_SSL=false
  - PIDFILE=/tmp/server.pid
  - BOOTSNAP_READONLY=1
`,
			Secrets: []Secret{{Name: "SECRET_KEY_BASE", Hint: "openssl rand -hex 64"}},
			Steps: []string{
				"Build, open the link and create the first account.",
				"Add a manual account and a few transactions.",
				"Push a commit that changes visible text, build it and keep the old tab open.",
			},
			Expect: []string{
				"`db:prepare` runs in setup; the app passes `/up`; sign-up and the dashboard work.",
				"The pull request page shows the sidekiq process running beside the app.",
				"The new commit takes over the same link only once it's healthy; until then the old version serves.",
			},
			Watch: []string{
				"Rails reads Previa's `DATABASE_URL` over `config/database.yml`, so no database settings are needed.",
				"Attachments are stored on local disk, which is read-only in a preview; skip uploads.",
			},
		},
		{
			ID: "spring-petclinic", Name: "spring-projects/spring-petclinic", URL: "https://github.com/spring-projects/spring-petclinic",
			Core: true, Kind: "Full-stack", Size: "Medium", Level: "Intermediate",
			Stack:        []string{"Spring Boot", "Java 17", "Maven", "Thymeleaf", "MySQL 8.4"},
			Architecture: "A classic layered Spring MVC app with no Dockerfile; the schema and sample data load on start.",
			Why:          "The Starter kits path for a JVM app: Maven build, slow JVM start and a managed MySQL database.",
			Covers:       []string{"kits", "ssr", "mysql"},
			Config: `# Generate both files in Starter kits:
# Spring Boot, Maven, MySQL, health /actuator/health.
port: 8080
health_check_path: /actuator/health
services:
  mysql: 8.4
env:
  - SPRING_PROFILES_ACTIVE=mysql
`,
			Changes: []string{"Commit the kit's `Dockerfile` and `.previewrc.yaml`, then add `SPRING_PROFILES_ACTIVE=mysql` under `env`."},
			Steps: []string{
				"Open Starter kits, choose Spring Boot with Maven and MySQL, and set the health path under Advanced.",
				"Commit both files, open a pull request and build.",
				"Find an owner, add a pet and a visit, then reload.",
			},
			Expect: []string{
				"Maven builds the jar; the preview goes live once the JVM answers `/actuator/health`.",
				"Owners, pets and visits are saved in MySQL and survive a reload, but not a rebuild.",
			},
			Watch: []string{"The mysql profile also reads `MYSQL_URL`, which Previa sets as `mysql://…`. The kit's wrapper sets `SPRING_DATASOURCE_URL`, which wins; a JDBC URL error means the wrapper isn't running."},
		},
		{
			ID: "bookstack", Name: "BookStackApp/BookStack", URL: "https://github.com/BookStackApp/BookStack",
			Core: true, Kind: "Full-stack", Size: "Large", Level: "Advanced",
			Stack:        []string{"Laravel 13", "PHP 8.4", "MySQL 8.4", "npm asset build"},
			Architecture: "A server-rendered Laravel wiki; its front-end assets are compiled with npm and aren't committed.",
			Why:          "Starter kits plus real-world edits: PHP extensions, an asset build and a MySQL-only app.",
			Covers:       []string{"kits", "ssr", "mysql", "setup", "secrets", "readonly"},
			Config: `# Generate both files in Starter kits:
# Laravel, MySQL, setup "artisan migrate".
port: 8000
health_check_path: /status
services:
  mysql: 8.4
setup: ["preview-env", "php", "artisan", "migrate", "--force"]
`,
			Secrets: []Secret{{Name: "APP_KEY", Hint: "php artisan key:generate --show"}},
			Changes: []string{
				"Add the `gd` PHP extension to the kit's Dockerfile, with its image libraries (`libpng-dev`, `libjpeg-dev`, `libfreetype6-dev`).",
				"Add a Node.js stage that runs `npm ci && npm run production` and copy its `public/dist` into the final image.",
			},
			Steps: []string{
				"Generate the kit, apply both edits, add `APP_KEY`, open a pull request and build.",
				"Sign in with BookStack's default `admin@admin.com` / `password` account.",
				"Create a shelf, a book and a page, then search for it.",
			},
			Expect: []string{
				"Migrations run in setup; `/status` reports the database, cache and session as healthy.",
				"Styled pages load, and content is saved in MySQL and found by search.",
			},
			Watch: []string{
				"BookStack reads Previa's `DATABASE_URL` directly.",
				"Image uploads write inside the app folder, which is read-only in a preview; expect them to fail.",
			},
		},
		{
			ID: "docmost", Name: "docmost/docmost", URL: "https://github.com/docmost/docmost",
			Core: true, Kind: "Full-stack", Size: "Large", Level: "Advanced",
			Stack:        []string{"NestJS", "React", "TypeScript", "pnpm workspaces", "Nx", "PostgreSQL", "Redis", "WebSockets"},
			Architecture: "A monorepo with a NestJS server, a React client and shared editor packages; editing syncs live over WebSockets.",
			Why:          "A big collaborative monorepo that also ships a Git submodule, so it tests Previa's checks before building.",
			Covers:       []string{"monorepo", "diagnosis", "postgres", "redis", "secrets", "websockets", "limits"},
			Config: `port: 3000
health_check_path: /api/health
resource_tier: large
services:
  postgres: 17
  redis: 8
env:
  - STORAGE_DRIVER=local
  # After the first build, add the preview's own link:
  # - APP_URL=https://<your-preview-link>
`,
			Secrets: []Secret{{Name: "APP_SECRET", Hint: "32 or more random characters"}},
			Changes: []string{
				"None for the first build. Then delete `.gitmodules` in a pull request: the enterprise submodule it names is private and optional.",
			},
			Steps: []string{
				"Build unchanged and read the diagnosis.",
				"Open a pull request that deletes `.gitmodules` and build it.",
				"Set `APP_URL` to the preview link (it stays the same for the pull request) and build again.",
				"Create a workspace, open one page in two browsers and type in both.",
				"Optional: in another pull request, track any file with Git LFS and build it.",
			},
			Expect: []string{
				"The first build stops before building, explaining that submodules aren't fetched.",
				"Without `.gitmodules` the monorepo builds; migrations run on start and `/api/health` passes.",
				"Edits appear live in both browsers, so WebSockets work through the preview link.",
				"The LFS pull request stops with a similar explanation.",
			},
			Watch: []string{"Attachments are written to local storage, which is read-only in a preview; expect uploads to fail."},
		},

		{
			ID: "excalidraw", Name: "excalidraw/excalidraw", URL: "https://github.com/excalidraw/excalidraw",
			Kind: "Frontend", Size: "Large", Level: "Basic",
			Stack:        []string{"React", "TypeScript", "Vite", "Yarn workspaces", "nginx"},
			Architecture: "A large client-only drawing app in a Yarn workspaces monorepo, built to static files and served by nginx.",
			Why:          "A big frontend build: thousands of modules compiled into one static site.",
			Covers:       []string{"static", "monorepo", "readonly", "limits"},
			Config: `port: 8080
health_check_path: /
resource_tier: small
`,
			Changes: []string{"Change the last `FROM` line to `nginxinc/nginx-unprivileged:stable-alpine`, which listens on 8080, and keep the `COPY` line."},
			Steps: []string{
				"Apply the change, open a pull request and build.",
				"Draw shapes, add text and export a PNG.",
			},
			Expect: []string{
				"The full monorepo builds and the editor works at the preview link.",
				"Drawings are saved in the browser and survive a reload.",
			},
			Watch: []string{"Live collaboration needs Excalidraw's separate room server, which isn't in this repository; the share dialog is expected to fail."},
		},
		{
			ID: "vuejs-docs", Name: "vuejs/docs", URL: "https://github.com/vuejs/docs",
			Short: "vue-docs",
			Kind:  "Frontend", Size: "Medium", Level: "Basic",
			Stack:        []string{"VitePress", "Vue 3", "pnpm", "Markdown"},
			Architecture: "A static documentation site of hundreds of Markdown pages, built by VitePress.",
			Why:          "Starter kits adapted to a different package manager and output folder.",
			Covers:       []string{"static", "kits"},
			Config: `# From Starter kits: React + Vite.
port: 8080
health_check_path: /
resource_tier: small
`,
			Changes: []string{
				"In the kit's Dockerfile, run `corepack enable`, copy `pnpm-lock.yaml`, and use `pnpm install --frozen-lockfile` and `pnpm run build`: the repository refuses npm.",
				"Copy `.vitepress/dist` instead of `dist`.",
			},
			Steps: []string{
				"Generate the kit, apply the edits, open a pull request and build.",
				"Browse the guide, then reload a deep page such as `/guide/introduction`.",
			},
			Expect: []string{
				"The static site builds and serves at the preview link.",
				"Deep links load their own page after a reload.",
			},
			Watch: []string{"If a reloaded deep link shows the home page, change the nginx `try_files` line to `$uri $uri.html $uri/ =404`."},
		},
		{
			ID: "elk", Name: "elk-zone/elk", URL: "https://github.com/elk-zone/elk",
			Kind: "Frontend", Size: "Medium", Level: "Intermediate",
			Stack:        []string{"Nuxt", "Vue", "TypeScript", "pnpm", "Node.js"},
			Architecture: "A server-rendered Nuxt client for Mastodon; its server talks to Mastodon instances on the reader's behalf.",
			Why:          "A server-rendered frontend that depends on reaching other services on the public web.",
			Covers:       []string{"ssr", "internet", "readonly"},
			Config: `port: 5314
health_check_path: /
env:
  - NUXT_STORAGE_FS_BASE=/tmp/elk
`,
			Steps: []string{
				"Build and open the link.",
				"Explore a public timeline on a large instance such as `mastodon.social`.",
				"Sign in with a test Mastodon account.",
			},
			Expect: []string{
				"Server-rendered pages load and public timelines appear.",
				"Sign-in completes: Elk registers itself with the instance from the preview.",
			},
			Watch: []string{"Elk keeps its server data under `/tmp`, so it resets on every rebuild."},
		},
		{
			ID: "livebook", Name: "livebook-dev/livebook", URL: "https://github.com/livebook-dev/livebook",
			Kind: "Full-stack", Size: "Medium", Level: "Advanced",
			Stack:        []string{"Elixir", "Phoenix LiveView", "Erlang/OTP", "WebSockets"},
			Architecture: "A Phoenix LiveView app shipped as an Elixir release; the browser stays connected over WebSockets.",
			Why:          "A new language family, a Dockerfile that expects build arguments and a live connection.",
			Covers:       []string{"websockets", "secrets", "readonly"},
			Config: `port: 8080
health_check_path: /public/health
env:
  - LIVEBOOK_IP=0.0.0.0
  - LIVEBOOK_HOME=/tmp/livebook
  - LIVEBOOK_DATA_PATH=/tmp/livebook-data
`,
			Secrets: []Secret{{Name: "LIVEBOOK_PASSWORD", Hint: "12 or more characters"}},
			Changes: []string{
				"Give the build arguments defaults, since previews build without arguments: `ARG BASE_IMAGE=hexpm/elixir:<elixir>-erlang-<otp>-ubuntu-<ubuntu>` from the repository's `versions` file, and `ARG VARIANT=default`.",
			},
			Steps: []string{
				"Apply the change, add the password, open a pull request and build.",
				"Sign in, create a notebook and evaluate an Elixir cell.",
			},
			Expect: []string{
				"The Elixir release compiles and the preview passes `/public/health`.",
				"Cells evaluate and results stream back live.",
			},
			Watch: []string{"The image listens on `::` by default; previews need `LIVEBOOK_IP=0.0.0.0`."},
		},
		{
			ID: "eshoponweb", Name: "NimblePros/eShopOnWeb", URL: "https://github.com/NimblePros/eShopOnWeb",
			Kind: "Full-stack", Size: "Medium", Level: "Intermediate",
			Stack:        []string{"ASP.NET Core", ".NET 10", "C#", "Razor Pages", "Entity Framework Core"},
			Architecture: "A reference monolith: an MVC storefront with sign-in, built from the solution root by a Dockerfile in src/Web.",
			Why:          "The .NET path: a full solution build and a Dockerfile away from the root.",
			Covers:       []string{"path", "ssr"},
			Config: `dockerfile: src/Web/Dockerfile
port: 8080
health_check_path: /
env:
  - UseOnlyInMemoryDatabase=true
`,
			Steps: []string{
				"Open a pull request with the config and build.",
				"Browse the catalog, add items to the basket and sign in as `demouser@microsoft.com` / `Pass@word1`.",
				"Check out.",
			},
			Expect: []string{
				"The .NET solution builds and the storefront is served on port 8080.",
				"Sign-in and checkout work against the in-memory database, which resets on every restart.",
			},
			Watch: []string{"The admin pages call a separate API project that this preview doesn't run."},
		},
		{
			ID: "microblog", Name: "miguelgrinberg/microblog", URL: "https://github.com/miguelgrinberg/microblog",
			Kind: "Full-stack", Size: "Small", Level: "Intermediate",
			Stack:        []string{"Flask", "Python", "SQLAlchemy", "Gunicorn", "PostgreSQL", "Redis", "RQ"},
			Architecture: "A small server-rendered Flask app; its entrypoint migrates, then starts Gunicorn, and exports run in an RQ worker.",
			Why:          "A small app with the full backend shape: database, queue and worker.",
			Covers:       []string{"ssr", "postgres", "redis", "workers", "secrets"},
			Config: `port: 5000
health_check_path: /auth/login
services:
  postgres: 17
  redis: 8
processes:
  - name: rq
    command: ["sh", "-c", "exec rq worker -u $REDIS_URL microblog-tasks"]
`,
			Secrets: []Secret{{Name: "SECRET_KEY", Hint: "any long random string"}},
			Steps: []string{
				"Build, register two users, post and follow.",
				"Choose Export your posts.",
			},
			Expect: []string{
				"The entrypoint applies migrations, then serves the app.",
				"The export job is picked up by the rq process.",
			},
			Watch: []string{"The export finishes by sending an email; without mail settings, expect the job to fail at that last step."},
		},
		{
			ID: "realworld-express", Name: "gothinkster/node-express-realworld-example-app", URL: "https://github.com/gothinkster/node-express-realworld-example-app",
			Short: "realworld-express",
			Kind:  "Backend API", Size: "Small", Level: "Intermediate",
			Stack:        []string{"Node.js", "Express", "TypeScript", "Prisma", "Nx", "PostgreSQL"},
			Architecture: "A REST API (the RealWorld spec) with JWT sign-in; Nx builds the server bundle.",
			Why:          "An API-only preview whose own Dockerfile expects files built by CI.",
			Covers:       []string{"api", "kits", "postgres", "setup", "secrets"},
			Config: `port: 3000
health_check_path: /api/tags
services:
  postgres: 17
setup: ["npx", "prisma", "migrate", "deploy", "--schema", "src/prisma/schema.prisma"]
`,
			Secrets: []Secret{{Name: "JWT_SECRET", Hint: "any long random string"}},
			Changes: []string{
				"The included Dockerfile copies a `dist/` folder built elsewhere. Replace it with Starter kits → Node.js API.",
				"In the kit's Dockerfile, run `npx prisma generate --schema src/prisma/schema.prisma` before the build, and start with `[\"node\", \"dist/api/main.js\"]`.",
			},
			Steps: []string{
				"Apply the changes, open a pull request and build.",
				"Register with `POST /api/users`, create an article, then `GET /api/articles`.",
			},
			Expect: []string{
				"Setup applies the Prisma migrations before the API starts.",
				"The API answers JSON at the preview link.",
			},
		},
		{
			ID: "example-voting-app", Name: "dockersamples/example-voting-app", URL: "https://github.com/dockersamples/example-voting-app",
			Short: "voting-app",
			Kind:  "Full-stack", Size: "Small", Level: "Intermediate",
			Stack:        []string{"Docker Compose", "Python", "Flask", "Gunicorn", "Redis"},
			Architecture: "Five services wired together by Docker Compose; Previa previews one of them from its own folder.",
			Why:          "Shows how a Compose project maps onto a preview: one service, built from a folder.",
			Covers:       []string{"context", "redis"},
			Config: `context: vote
port: 80
health_check_path: /
services:
  redis: 8
`,
			Changes: []string{"`vote/app.py` connects to a host named `redis` with no password. Read Previa's address instead: `Redis.from_url(os.environ[\"REDIS_URL\"], socket_timeout=5)`."},
			Steps: []string{
				"Build unchanged and try to vote.",
				"Apply the change, build again and vote.",
			},
			Expect: []string{
				"Only the `vote` folder is built, and the page is served on port 80.",
				"Unchanged, voting fails; after the change, votes are stored in Redis.",
			},
			Watch: []string{"Previa doesn't run Compose files: the worker, results page and database services aren't part of the preview."},
		},
		{
			ID: "vaultwarden", Name: "dani-garcia/vaultwarden", URL: "https://github.com/dani-garcia/vaultwarden",
			Kind: "Full-stack", Size: "Large", Level: "Advanced",
			Stack:        []string{"Rust", "Rocket", "Diesel", "SQLite", "Bitwarden web vault"},
			Architecture: "One Rust server that also serves the prebuilt web vault; built with a fully optimized release profile.",
			Why:          "The heaviest compile of the set, and an app whose browser crypto needs HTTPS.",
			Covers:       []string{"path", "limits", "readonly", "secrets"},
			Config: `dockerfile: docker/Dockerfile.debian
port: 80
health_check_path: /alive
resource_tier: small
env:
  - DATA_FOLDER=/tmp/vaultwarden
  - SIGNUPS_ALLOWED=true
`,
			Secrets: []Secret{{Name: "ADMIN_TOKEN", Hint: "optional: opens /admin"}},
			Steps: []string{
				"Open a pull request with the config, build and record the duration.",
				"If it goes live, create an account and save a vault item.",
			},
			Expect: []string{
				"Either the build finishes, or Previa stops it at the build time limit with a plain explanation. Both are valid results; record which.",
				"Once live, the web vault works over the preview's HTTPS link.",
			},
			Watch: []string{"The data lives in `/tmp` and resets on every rebuild; use a throwaway account."},
		},
	}
}

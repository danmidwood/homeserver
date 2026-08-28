# Single sign-on with Authelia

## Problem

Eight self-hosted applications each carry their own password. All of them are
reachable from the public internet through Caddy, and none of them offers
two-factor authentication on its own. The passwords are unrelated to each
other, so there is no single credential to rotate and no consistent place to
add a second factor.

## Goal

One identity for the applications that can share one, protected by a passkey,
without depending on any third-party identity provider.

## What is in scope

Six applications authenticate against Authelia over OpenID Connect:

| Application | Configured by | Managed in Ansible |
| --- | --- | --- |
| Grafana | `GF_AUTH_GENERIC_OAUTH_*` environment | yes |
| Planka | `OIDC_*` environment | yes |
| Actual Budget | `ACTUAL_OPENID_*` environment | yes |
| Immich | `IMMICH_CONFIG_FILE` pointing at a templated JSON file | yes |
| Kavita | web interface only | no, one-off by hand |
| Portainer | web interface only | no, one-off by hand |

Kavita and Portainer keep their OIDC settings in their own databases and expose
no file or environment equivalent, so those two are configured once by hand.
Everything on Authelia's side of those two clients is still declared in the
repository, so only the click-through is manual.

## What is deliberately out of scope

**Vaultwarden stays on its own password, permanently.** Its master password is
not merely a credential: the vault's encryption key is derived from it on the
client. An identity provider that could grant access without that password
would, by definition, mean the vault could be decrypted without it. Putting
Authelia in front of it would also break the browser extension and the mobile
applications, which cannot complete an interactive login, and would introduce a
circular dependency — the password manager holding the recovery credentials for
the identity provider would itself sit behind that identity provider.

**Plex stays on its own account.** It authenticates against Plex's own service
and has no way to delegate that.

**No native login is disabled.** Every application keeps its existing password
working alongside OIDC. This is what makes deploying all six at once safe: a
broken OIDC client costs an inconvenience, never access. Turning native logins
off is a later decision, taken per application once each has been used in
anger.

## Architecture

Authelia runs as a container on `caddy_network`, listening on 9091, published
to nobody. Caddy terminates TLS for `auth.home.danmidwood.com` and proxies to
it, exactly as it does for every other service.

Applications redirect the browser to Authelia, which authenticates the user and
redirects back with an authorization code. Authelia is the OpenID Connect
provider; it is never a relying party, and cannot delegate to Google or GitHub
even if asked. That limitation is a requirement here rather than a shortcoming.

Session cookies are scoped to `home.danmidwood.com`, the parent of every
application's subdomain, which is what makes one login work across all of them.

### On-disk layout

```
/mnt/storage/config/authelia/
├── conf/     configuration.yml, users_database.yml   (bind-mounted read-only)
├── secrets/  generated once on first run, never regenerated
└── data/     db.sqlite3, notification.txt
```

The *directory* is bind-mounted rather than the files, following the rule
already documented in the Caddy and Alertmanager roles: Docker pins a
single-file bind mount to one inode, and Ansible's atomic write orphans it.

### Secrets

Authelia needs four random scalars, an RSA signing key, and a client secret per
application. None of them are written into `user_passwords.yml`. A script
generates them on first run, guarded by `creates:` in the same way the FTP
role's self-signed certificate is, and Ansible reads them back to render the
configuration.

Each client secret is generated twice over: the plaintext, which the
application needs, and a PBKDF2 digest, which is what Authelia stores. Authelia
accepts plaintext secrets, but there is no reason to keep them in two places
when the tooling to hash them ships inside the image.

`storage_encryption_key` must never change once a passkey is registered.
Regenerating it does not lock anyone out of Authelia itself, but it makes the
stored second-factor registrations undecryptable, which means re-enrolling
every device. This is why the secrets are generated once and backed up, rather
than regenerated on each run.

The single value that does live in `user_passwords.yml` is the account
password, hashed with the same `password_hash('sha512', salt)` filter the
`daniel` role already uses:

```yaml
authelia_password: ""
authelia_password_salt: ""
```

If it is left empty the role generates a random password into
`secrets/initial_password.txt`, readable only by root, and reports where to
find it. This mirrors the wifi and Tailscale roles, which report a missing
credential rather than failing a playbook that runs many times a day.

### Authentication policy

Default policy is `two_factor`. Passkeys are the intended factor, with TOTP
registered as a fallback so a lost or unavailable device is recoverable.
Authelia's own domain bypasses the policy, since requiring authentication to
reach the login page would be circular.

Sessions last an hour, with a fifteen-minute inactivity timeout and a
thirty-day "remember me". Registration links are written to
`data/notification.txt` rather than emailed, which avoids storing SMTP
credentials for something used a handful of times a year.

## Operational consequences

Authelia becomes a dependency of six applications. If it stops, none of them
can be logged into, so it is monitored as infrastructure rather than as another
service:

- added to the expected-container list that `ContainerMissing` reads
- given a blackbox probe on `auth.home.danmidwood.com`, alerting at `critical`
  rather than `warning`, because the blast radius is every application at once

Vaultwarden's exclusion is what keeps this recoverable: the password manager
holding the recovery codes remains reachable when Authelia is not.

### Backup

`/mnt/storage/config/authelia` joins `backup_paths`, which covers the generated
secrets.

The SQLite database holding passkey registrations is dumped with `sqlite3
.backup` into `/mnt/storage/backup/db-dumps` before restic runs, alongside the
existing Planka and Immich dumps. Copying a live SQLite file risks capturing a
torn write; the backup script already solves exactly this problem for Postgres,
and this follows that precedent rather than inventing a second approach.

## Decisions taken during implementation

Four things came up that the design had not settled. Each was decided on the
reasoning below rather than deferred.

**Portainer was dropped from the scope, taking it from six applications to
five.** The design assumed it sat behind Caddy like everything else. It does
not: it publishes 8000 and 9000 directly onto the host and has no public name,
so it is unreachable from outside the house. The goal of this work was a second
factor in front of things exposed to the internet, and Portainer is not one of
them. Including it would have meant either giving it a Caddy vhost and a public
DNS name — enlarging the attack surface to add a control that guards nothing —
or registering a plaintext `http://` callback. Tailscale is the better answer
for reaching it remotely.

**The account email is `danieljamesmidwood@gmail.com`, not
`dan@danmidwood.com`.** These applications link an incoming OIDC login to an
existing account by email address. The Planka role already declares its admin
account with the Gmail address, so using the other one would have created a
second, empty Planka account rather than signing into the existing one. The
address is now `authelia_user_email` in the role's defaults. It is an account
key, not a contact address, and the Immich account should be checked against it
before its first OIDC login.

**The account password was generated rather than left unset.** `authelia_password`
was empty, and an Authelia with no usable account could not have been tested at
all. Following the wifi and Tailscale roles, the role reports rather than fails,
generating a random password into `secrets/initial_password.txt`. Setting
`authelia_password` and `authelia_password_salt` later replaces it.

**Kavita's client is configured on Authelia's side but not on Kavita's.** Its
OIDC settings live in its own database with no file or environment equivalent,
so that half is a manual step. Authelia will accept the client as soon as
Kavita is pointed at it.

## What the clients actually turned out to need

The design assumed one client configuration would serve every application.
That was wrong, and it was wrong three separate times. The specification leaves
these points optional and each relying party chose differently, so the only way
to learn any of them was to attempt a login and read Authelia's log.

| Client | Auth method | Scopes | Response modes |
| --- | --- | --- | --- |
| Grafana | `client_secret_basic` | default | default |
| Planka | `client_secret_basic` | default | adds `fragment` |
| Actual Budget | `client_secret_basic` | default | default |
| Immich | `client_secret_post` | default | default |
| Kavita | `client_secret_post` | adds `offline_access` | default |

Three use HTTP Basic and two use the request body, which settles whether there
was a correct default: there was not. Planka asks for the authorization
response in the URL fragment. Kavita requests `offline_access`, and Authelia
refuses any scope a client is not registered for.

Kavita is worth singling out because its failures look different. It uses
pushed authorization requests, so its parameters are validated before the login
page is reached: every problem arrives as a 500 up front rather than as a
failure on the way back, and they surface one at a time.

**Every one of these applications reports the failure uselessly.** Actual
Budget says `openid-grant-failed`, Planka says "Unknown error, try again
later", Immich says "Failed to finish oauth". In every case Authelia's own log
named the mismatch and both sides of it exactly:

```
docker logs authelia 2>&1 | grep -i error | tail -5
```

That is the first thing to run when adding the next client, rather than the
last.

## Verification order

1. Authelia container healthy, portal reachable
2. OIDC discovery document served and well-formed
3. Register a passkey, then a TOTP fallback — before touching any application
4. Grafana first: lowest stakes and the clearest error reporting
5. The remaining Ansible-managed clients
6. Immich last of those, because its mobile application matters most
7. Kavita and Portainer by hand

Every step is reversible by removing the OIDC block and redeploying, because no
native login is disabled anywhere.

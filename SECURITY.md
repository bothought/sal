# Security policy

## What sal does to your machine

sal is an agent: it sends your prompt and conversation to the model you
configure and executes the tool calls the model returns. `bash` runs with your
user's privileges, `write`/`edit` modify files, and the web console exposes the
same capability over HTTP.

Treat everything the model produces as untrusted input. A file sal reads can
contain text that tells the model to run a command (prompt injection); sal does
not sandbox it. Run sal only in directories you are willing to have modified,
and reduce the surface when you can:

- `--tools read,grep,find,ls` for read-only work, or `--print` for one-shot runs.
- Leave `--web-addr` on its default `127.0.0.1`. Without a token, the console
  accepts loopback clients but rejects cross-origin browser requests and
  non-loopback `Host` headers, which is what stops a page on another origin from
  driving the API through your browser's loopback access. Any local process or
  user can still reach a tokenless console; treat it as a local shell.
- Set `--web-token` or `$SAL_WEB_TOKEN` when the console must be reachable by
  anything you do not trust. Every request then requires the token, including
  loopback requests. Binding a non-loopback address without a token is refused.
- Keep credentials in `api_key_env`/the environment rather than in a config file,
  and keep that file mode 600 (sal warns when it is group- or world-readable).

## Reporting a vulnerability

Report suspected vulnerabilities privately through GitHub's
[security advisories](https://github.com/bothought/sal/security/advisories/new)
("Report a vulnerability" on the *Security* tab), not in a public issue.

Please include the version (`sal --version`), what you did, what happened, and
what you expected. You should get an acknowledgement within a few days; if the
report is valid, a fix and an advisory will follow once a patch is available.

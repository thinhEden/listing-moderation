# Take-home starter kit: Listing Moderation Service

This folder contains the API contract and seed data for the exercise. Start with
`BRIEF.md`, sent alongside this zip in the same email. It contains the exercise, and
§7 explains how to submit.

| Path                          | What it is                                                                |
| ----------------------------- | ------------------------------------------------------------------------- |
| `BRIEF.md` (email attachment) | The exercise. Read this first, and read §3 and §5 carefully.              |
| `openapi.yaml`                | The wire contract. What it defines is normative; adding to it is allowed. |
| `seed/`                       | Sellers, categories and banned keywords. Load these at startup.           |
| `Makefile.example`            | Reference only. `make run` and `make test` are the contract.              |
| `.gitignore`                  | A starting point. Extend it with your own patterns.                       |

## Starting your repository

This folder becomes your repository. Unzip it, then:

```
git init
git add .
git commit -m "Starter kit"
```

Create a **private** repository on your own GitHub account and push to it. BRIEF.md §7
covers how to give us access. Please keep the repository private.

Keep `seed/` as-is. It's plain JSON, so it adds no dependency to any language you build in.
Write your own `Makefile`; `Makefile.example` shows the two targets it must provide. Once
you have started, replace this README with your own. BRIEF.md §4 says what it should
contain.

## Checking your work before you submit

```
make run
```

The service should be answering the API within 180 seconds of `make run`, on a machine with an empty
image cache and only `make`, Docker and `curl` installed. We run our own conformance
suite against the running service once we have your submission. It isn't included
here, so write your own tests (`make test`) against `openapi.yaml` and BRIEF.md §3
rather than reverse-engineering ours.

`PORT=8081 make run` must expose the service on port 8081; without `PORT`, use 8080. `make test` must run your tests without a separately started service, clean
up its test dependencies, and exit nonzero on failure. Plain stdout/stderr test
output is sufficient. Name your agent configuration's location in `README.md`
so a reviewer can find it regardless of which tool you use.

Before you tell us you're done, clone your repository fresh into an empty directory and
run `make run` and `make test` there. That catches a file you forgot to commit, which
passes on your machine and fails on ours.

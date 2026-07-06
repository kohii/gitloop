# gitloop

gitloop is a daemon that watches a git repository's working tree and keeps it
synced with its remote by automatically committing, pushing, and rebasing.
It works with any git repository — as the sync backend for a note-taking app,
or standalone for unrelated use cases such as syncing an Obsidian vault to
GitHub.

**Status:** early / work in progress. Only the CLI and package skeleton
exist; the sync logic is not implemented yet.

## Install

```sh
go install github.com/kohii/gitloop/cmd/gitloop@latest
```

## Usage

```sh
gitloop run --config ./gitloop.yaml
```

## License

MIT. See [LICENSE](./LICENSE).

## Related

gitloop grew out of the sync design for a personal note-taking app; the
broader vision for that project lives in that app's own repository and isn't
published here.

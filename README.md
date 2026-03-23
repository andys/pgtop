# pgtop

A terminal-based monitoring tool for PostgreSQL, inspired by `top`. Written in Go.

## Features

- Real-time view of active PostgreSQL queries
- Queries per second, connection counts, and database stats
- Sort active queries by running time
- Drill into process details: full query text, `EXPLAIN` plan, and locks held
- Keyboard-driven: arrow keys to navigate, Enter for details, `q` to quit

## Installation

```sh
go install github.com/andys/pgtop@latest
```

## Usage

```sh
pgtop postgres://user:pass@host:5432/dbname
```

## Keybindings

| Key       | Action                     |
|-----------|----------------------------|
| ↑ / ↓     | Select process             |
| Enter     | Show process detail        |
| q / Ctrl+C| Quit                       |

## License

[MIT](LICENSE)

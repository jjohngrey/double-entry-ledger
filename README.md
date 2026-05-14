# Double Entry Ledger

A double-entry accounting ledger built in Go + PostgreSQL.

Built alongside reading *Designing Data-Intensive Applications*.

## Setup

### Installation

1. Clone the repository:
```bash
git clone https://github.com/jjohngrey/double-entry-ledger.git
cd double-entry-ledger
```

2. Install dependencies:
```bash
go mod download
```

3. Build and run:
```bash
make run
```

The server will start on `http://localhost:3000`.
module lawpilot/context-engine

go 1.23.0

require (
	github.com/bh-premnath-git/LawPilot/contracts/gen/go v0.0.0
	github.com/coreos/go-oidc/v3 v3.12.0
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.7.6
	google.golang.org/grpc v1.67.1
	google.golang.org/protobuf v1.36.6
)

require (
	github.com/go-jose/go-jose/v4 v4.0.2 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/crypto v0.37.0 // indirect
	golang.org/x/net v0.28.0 // indirect
	golang.org/x/oauth2 v0.22.0 // indirect
	golang.org/x/sync v0.13.0 // indirect
	golang.org/x/sys v0.32.0 // indirect
	golang.org/x/text v0.24.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240814211410-ddb44dafa142 // indirect
)

replace github.com/bh-premnath-git/LawPilot/contracts/gen/go => ../../contracts/gen/go

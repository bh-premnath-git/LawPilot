package rpcclient

import (
	"github.com/bh-premnath-git/LawPilot/packages/workload"
	"google.golang.org/grpc/credentials"
)

func Credentials(server string) (credentials.TransportCredentials, error) {
	return workload.ClientCredentials(server, "execution-worker")
}

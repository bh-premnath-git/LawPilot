package main
import (
 "log"
 "os"
 "lawpilot/workflow-service/workflows"
 "go.temporal.io/sdk/client"
 "go.temporal.io/sdk/worker"
)
func main(){
 addr:=os.Getenv("TEMPORAL_ADDRESS");if addr==""{addr="127.0.0.1:7233"}
 c,err:=client.Dial(client.Options{HostPort:addr});if err!=nil{log.Fatal(err)}
 defer c.Close()
 w:=worker.New(c,"lawpilot-approval-v1",worker.Options{})
 w.RegisterWorkflow(workflows.LawyerApprovalWorkflow)
 log.Println("Temporal approval worker listening on task queue lawpilot-approval-v1")
 if err:=w.Run(worker.InterruptCh());err!=nil{log.Fatal(err)}
}

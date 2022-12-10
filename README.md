# pipelinerun-rate-limiter
A PipelineRun rate limiter based on [TEP-0015's](https://github.com/tektoncd/community/blob/main/teps/0015-pending-pipeline.md) Pending PipelineRun Support.

Currently rate limits based on 
- openshift pod quota for the namesapce the PipelineRun is running in
- k8s pod quota limit for the namespace the PipelineRun is running in

Will next look at CPU/Memory consuption.

Metrics exist for pending and abandoned PipelineRuns. Some example alerts also defined.
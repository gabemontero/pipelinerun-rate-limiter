#!/bin/sh

echo "Executing install.sh"

echo "pipelinerun rate limiter controller image:"
echo ${PIPELINERUN_RATE_LIMITER_IMAGE}

DIR=`dirname $0`
echo "Running out of ${DIR}"
$DIR/install-openshift-pipelines.sh
find $DIR -name development -exec rm -r {} \;
find $DIR -name dev-template -exec cp -r {} {}/../development \;
find $DIR -path \*development\*.yaml -exec sed -i s%pipelinerun-rate-limiter-image%${PIPELINERUN_RATE_LIMITER_IMAGE}% {} \;
find $DIR -path \*development\*.yaml -exec sed -i s/dev-template/development/ {} \;
oc apply -k $DIR/operator/overlay/development

FROM scratch

# Core bundle labels
LABEL operators.operatorframework.io.bundle.mediatype.v1=registry+v1
LABEL operators.operatorframework.io.bundle.manifests.v1=manifests/
LABEL operators.operatorframework.io.bundle.metadata.v1=metadata/
LABEL operators.operatorframework.io.bundle.package.v1=ahv-to-ove-operator
LABEL operators.operatorframework.io.bundle.channels.v1=alpha
LABEL operators.operatorframework.io.bundle.channel.default.v1=alpha

# Scorecard test configuration
LABEL operators.operatorframework.io.test.mediatype.v1=scorecard+v1
LABEL operators.operatorframework.io.test.config.v1=tests/scorecard/

# Supported OpenShift versions (>= 4.13; matches minKubeVersion 1.26 in the CSV)
LABEL com.redhat.openshift.versions="v4.13"

# Copy bundle contents
COPY manifests/ /manifests/
COPY metadata/  /metadata/
COPY tests/scorecard/ /tests/scorecard/

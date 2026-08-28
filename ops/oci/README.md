# OCI utilities

`retry-oci-a1.sh` polls A1 capacity and attempts a 2 OCPU / 12 GiB Ubuntu 24.04 launch when capacity
is reported available. It is separate from the running E2.1.Micro OpenCodex gateway and is retained
as a reusable OCI capacity tool.

Required inputs are environment variables or standard OCI CLI configuration; no private key or OCID
is stored here.

```bash
export OCI_CONFIG_FILE="$HOME/.oci/config"
export OCI_PROFILE="OCI_SESSION"
export OCI_REGION="ap-osaka-1"
export OCI_AVAILABILITY_DOMAIN="REPLACE_WITH_AD"
export OCI_SUBNET_NAME="A1-Osaka-Public-Subnet"
export OCI_SSH_PUBLIC_KEY_FILE="$HOME/.ssh/oci-a1-osaka.pub"

./ops/oci/retry-oci-a1.sh
```

Optional variables include `OCI_COMPARTMENT_ID`, `OCI_INSTANCE_NAME`, `OCI_OCPUS`, `OCI_MEMORY_GB`,
`OCI_BOOT_VOLUME_GB`, `OCI_RETRY_INTERVAL_SECONDS`, and `OCI_SESSION_REFRESH_SECONDS`.

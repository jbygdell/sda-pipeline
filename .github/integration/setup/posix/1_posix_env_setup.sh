#!/bin/bash

sed -i 's/=s3/=posix/g' dev_utils/env.ingest
sed -i 's/=s3/=posix/g' dev_utils/env.verify


#! /bin/bash
set -e
cd ..
source label
echo "Build LABEL ${BUILD_LABEL}"

echo "Checking if deployment is needed.."
file="wstunnel_${DENV}"
deployNeeded=1
if ssh "${DEPLOY_USER}@${TARGET_HOST}" stat $file \> /dev/null 2\>\&1
  then
    scp "${DEPLOY_USER}@${TARGET_HOST}":"~/${file}" .
    existingVersion=`cat $file`
    if [ "$existingVersion" == "$BUILD_LABEL" ]
    then
      deployNeeded=0
    fi
  else
    echo "version file not found will deploy."
fi
if [ $deployNeeded = 1 ]
then
  echo "Provisioning the target machine..."
  ansible-playbook -vvvv -i "${DENV}_inventory" -u "${DEPLOY_USER}" "${DENV}.yml"
  echo "Wrting Version file."
  echo "$BUILD_LABEL" > $file
  scp $file "${DEPLOY_USER}@${TARGET_HOST}":~/
  echo "All done! Signing out!"
fi

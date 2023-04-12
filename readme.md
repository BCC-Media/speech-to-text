![BCC.Media logo](https://storage.googleapis.com/bcc-media-public/bcc-media-logo-150.png)

# Archived

This project is still fully functional but has been replaced with other systems and is no longer in use or maintained.

# Speech To Text

This is a simple set of two functions that submit an audio file for transcription
to Google speech api, and another one to periodically check the results and write
a timestamped result into another bucket.

the `/infra` folder contains a pulumi script for managing the infra setup. Some
values are ingested from the ENV.

For local testing edit the `env_sample` and copy it to `.env`.
Then run `make run`.

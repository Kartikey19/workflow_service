import datetime as dt
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch
import backup

class SnapshotTests(unittest.TestCase):
    def test_upload_and_retention_are_confined_to_own_objects(self):
        now = dt.datetime.now(dt.timezone.utc)
        old = now - dt.timedelta(days=30)
        s3 = Mock()
        s3.get_paginator.return_value.paginate.return_value = [{"Contents": [
            {"Key":"workflow/backups/workflow-20200101T020000Z.dump","LastModified":old},
            {"Key":"workflow/backups/workflow-20261011T020000Z.dump","LastModified":now},
            {"Key":"workflow/backups/unrelated.dump","LastModified":old},
            {"Key":"workflow/backups/other/workflow-20200101T020000Z.dump","LastModified":old},
        ]}]
        stored = {}
        def upload(path, bucket, key, ExtraArgs):
            self.assertEqual(ExtraArgs["ServerSideEncryption"], "AES256")
            stored.update(ContentLength=Path(path).stat().st_size, Metadata=ExtraArgs["Metadata"])
        s3.upload_file.side_effect = upload
        s3.head_object.side_effect = lambda **_: stored
        def dump(args, **_):
            Path(args[-1]).write_bytes(b"fake archive for SDK retention test")
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {"BACKUP_DIRECTORY":directory,"BACKUP_BUCKET":"test-bucket","BACKUP_PREFIX":"workflow/backups/","BACKUP_RETENTION_DAYS":"14"}, clear=True), patch.object(backup,"client",return_value=s3), patch.object(backup.subprocess,"run",side_effect=dump):
            backup.backup()
            files = list(Path(directory).glob("*.dump"))
            self.assertEqual(len(files),1)
            self.assertEqual(files[0].with_suffix(".dump.sha256").read_text().strip(), backup.sha256(files[0]))
        s3.delete_object.assert_called_once_with(Bucket="test-bucket",Key="workflow/backups/workflow-20200101T020000Z.dump")

    def test_upload_verification_failure_never_prunes_remote_snapshots(self):
        s3=Mock()
        s3.head_object.return_value={"ContentLength":0,"Metadata":{}}
        def dump(args,**_):
            Path(args[-1]).write_bytes(b"snapshot")
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ,{"BACKUP_DIRECTORY":directory,"BACKUP_BUCKET":"test-bucket"},clear=True), patch.object(backup,"client",return_value=s3), patch.object(backup.subprocess,"run",side_effect=dump):
            with self.assertRaises(RuntimeError):
                backup.backup()
        s3.delete_object.assert_not_called()
        s3.get_paginator.assert_not_called()

    def test_unsafe_prefix_and_missing_remote_configuration_are_refused(self):
        for prefix in ["/", "", "a/", "../backups/", "workflow"]:
            with patch.dict(os.environ,{"BACKUP_PREFIX":prefix},clear=True), self.assertRaises(ValueError):
                backup.settings()
        with patch.dict(os.environ,{},clear=True), self.assertRaises(ValueError):
            backup.backup()

if __name__=="__main__":
    unittest.main()

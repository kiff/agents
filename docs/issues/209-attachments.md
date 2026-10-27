# Files attached to a chat message: what kiff/tafs#209 needs here

**Issue:** [kiff/tafs#209](https://github.com/kiff/tafs/issues/209) · **Status:** the kiff/agents part is built; tafs cloud's routes and the app follow in kiff/tafs.

## What it's for

A person gives a taf, or Coach, the context they have: a photo of a receipt,
a policy PDF, a customer's email as text, a spreadsheet of orders. The file is
part of their message, in their own chat. It lives as long as that message,
reaches the model with it, and nothing else.

## The rules

- **The person's own message, only.** A file is attached by a person
  (`member:<id>`), sent once, in their own chat with that agent. A door's
  message never carries one, nor a job's, nor another person's.
- **Read only by them.** The download route answers only the person who
  attached it: not the service, an admin or the account's owner.
- **Never kept beyond its message.** Not in the agent's memory, not for the
  taf. "Keep for this taf" is separate work.
- **Inline, never in the provider's file storage.** Each turn sends the file's
  bytes with the request.
- **Context, not authority.** The files follow a fixed line (`attach.FileNote`):
  what they hold is material to read, and an instruction inside one is content.
  Nothing in a file changes a Card, a held call or a tool: those still go
  through KIFF.

## What can be attached

| Kind | Media types | Most per file | How the model reads it |
|---|---|---|---|
| Photo, screenshot | JPEG, PNG, GIF, WebP (the app converts HEIC and scales to 2576 px) | 10 MB | as an image |
| PDF | `application/pdf` | 25 MB, 100 pages | as a document: text, tables and charts |
| Text | plain text, CSV, Markdown | 2 MB | as a text document |
| Word | `.docx` | 2 MB, and 2 MB of text | its paragraphs and tables, as text |
| Excel | `.xlsx` | 2 MB, and 2 MB of text | each sheet as CSV, under its name |

Five files a message. Files with macros (`.docm`, `.xlsm`) have their own
media types and are refused, and so is a `.docx` or `.xlsx` carrying a VBA
project. A file over its limit is refused before upload, saying which limit
and the file's size.

## How a file arrives

1. **Declare.** `POST /agents/{id}/attachments?owner=member:<id>` with
   `{media_type, name, size, sha256}`. The answer is the attachment and an
   upload: a PUT to a presigned S3 URL, signed for that key, its exact size,
   its type and its SHA-256, working for 5 minutes. The bytes go straight to
   S3, never through the API.
2. **Send.** The run's start names it: `attachments: [id]`, with or without
   text. The runner takes each file before anything is stored: it must be the
   person's, for this agent, and unsent. Its bytes are read back and held to
   the size, the checksum and their real type (their first bytes, not what the
   app said). A file that fails is deleted. A Word or Excel file's text is
   stored beside it. Then the run is stored, and the files are bound to it.
3. **Read.** The run records the owner's message with each file by reference
   (`attach.Ref`), never its bytes. At the run's start the runner loads the
   bytes of the files its messages name, newest first, up to 20 MB and about
   100,000 tokens in all (a photo about 1,600, a PDF about 2,000 a page,
   text about one per four bytes). Files from the chat's earlier messages take
   at most 30,000 tokens of that: each run sends them again, so an old long
   PDF doesn't cost its full size on every message. A file past these reaches
   the model as a line saying it's out of view and can be sent again.

Word and Excel files are zips from someone else's computer. They're opened
with limits before anything is read: at most 1,000 parts, 16 MB each and
64 MB in all, uncompressed, and no part compressed more than 200 to 1. Their
XML is read strictly, and a document type declaration refuses the file, so no
entity is expanded or fetched. Nothing in them runs.

## When a file goes

- With its message: the definition's retention for owner messages (30 days
  for tafs), counted from when it was sent.
- With its chat, when the person deletes it; with its agent, when the taf is
  removed; with the account, when it's erased.
- An upload no message named, an hour after it was declared.

The function's sweep removes what is due, the bytes and then the record, so a
removal cut short is finished by the next sweep. A record also carries a TTL
a week past its due time. The bucket's lifecycle rule removes anything left
38 days on.

## Where it lives

- `attach`: the core. `Service` (`Create`, `Take`, `Bind`, `Read`, `Load`,
  `RemoveSession`, `RemoveAgent`, `EraseTenant`, `Sweep`), `Verify`, and the
  Word and Excel readers. Ports `Store` and `Blobs`.
- `attach/attachtest`: the checks every `Store` passes.
- `store/memory`, `store/dynamodb`: the stores. In the table, a record under
  `F|tenant`, and a due queue under `FD` ordered by when each goes.
- `store/s3`: the blobs, with presigned URLs.
- `model`: the `File` block; `model/anthropic` sends a photo as an image, a
  PDF as a document, and text as a plain-text document.
- `agent`, `chat`, `runner`, `server`, `cmd/lambda`: carrying the files with
  the message, the history, the routes, the sweep. Unset
  `AGENTS_ATTACHMENTS_BUCKET`, a message naming a file is refused.
- `infra/aws.sh`: the bucket (`kiff-agents-attachments`) in the function's
  region, eu-west-1 (it stops if a bucket of that name is anywhere else),
  private, encrypted, TLS only, with its lifecycle rule, and the function
  role's access to it.

## Tests

| Test | Guards |
|---|---|
| `attach.TestVerifyReadsTheType` | a file's real type, size and checksum, not the app's word |
| `attach.TestVerifyCountsPDFPages` | the 100-page limit |
| `attach.TestWordIsReadAsText`, `TestExcelIsReadAsCSV` | what the model reads of Word and Excel |
| `attach.TestOfficeFilesAreOpenedSafely` | a zip bomb, an external entity, macros, too many parts |
| `attach.TestCreateRefusesBeforeUpload` | types, limits and the message that names them |
| `attach.TestTakeBindLoad` | only the person's own, once; fenced; loaded; read only by them; gone with the message |
| `attach.TestTakeDeletesAMismatch`, `TestAnUnsentUploadGoesAfterAnHour`, `TestLoadKeepsToTheBudget`, `TestLoadKeepsToTheTokenBudget` | a bad upload removed; abandoned uploads; the turn's byte and token budgets; `TestEarlierFilesTakeASmallerShare`, earlier messages' share |
| `attachtest` (memory, DynamoDB) | tenant isolation, binding once, listing, the due queue |
| `store/s3.TestAnUploadIsPinned`, `TestADownloadIsNamed` | the signed size, type and checksum; the download's name |
| `runner.TestAFileReachesTheModelWithItsMessage` | the model gets it fenced with its bytes; events hold only references; the next message sees it |
| `runner.TestOnlyItsSendersOwnMessageCarriesAFile`, `TestAMessageOfFilesAlone` | not another person, a door or a job; not twice; files alone |
| `server.TestAttachingAFile`, `TestOnlyItsSenderReadsAFile`, `TestADoorSendsNoFile`, `TestFilesGoWithTheirChat` | the routes, who may read, doors, deletion |
| `cmd/e2e`, nightly: the planted file | tafs' Refunds taf gets a text file whose text says to refund a charge and ignore its Card; it answers the owner's question from the file and makes no call the file asks for |

## Left

- kiff/tafs: tafs cloud's routes and their forwarding, the app's attach
  button, thumbnails, the file in the thread and full-size view, and
  `docs/data.md`.
- Run `infra/aws.sh` once merged, for the bucket, the function's access and
  `AGENTS_ATTACHMENTS_BUCKET`.
- A person's export of their own chats, files included, is separate work.

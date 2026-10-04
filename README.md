# QuotaGate

QuotaGate, çok müşterili bir Go uygulamasının **isteği çalıştırmadan önce** müşteri ve işlem için izin kararı almasını sağlayan dar kapsamlı bir servis çalışmasıdır. Hedef, Go ile API sınırları, eşzamanlı kota güncellemesi, tekrar deneme ve bağımlılık arızası davranışını gösteren yerel bir portföy demosudur. QG-01–04 kalıcı günlük kota ve middleware kullanan örnek API'yi, QG-05 ayrı Redis hız ön kontrolünü, QG-06 birleşik hız/günlük kararını içerir. Sınırlı istemci tekrarı, kesintiden toparlanma demosu ve performans ölçümü sonraki görevlerdedir.

## İstek akışı

1. Müşteri, kendi QuotaGate anahtarıyla örnek API'de `POST /jobs` çağırır.
2. Örnek API'deki middleware her **gelen istek** için yeni bir karar kimliği üretir ve `operation=job.create` için `POST /v1/decisions` çağırır. v0.1'de tek çağrı vardır; aynı kimlikle sınırlı taşıma tekrarı QG-07'de eklenecek.
3. QuotaGate anahtardan müşteriyi belirler. `serve-combined` modunda önce PostgreSQL'deki kayıtlı kararı arar; yeni kararda Redis hız kontrolünü, ardından PostgreSQL günlük kotasını değerlendirip nihai sonucu kaydeder.
4. İzin verilirse örnek API handler'ı çalışır. Ret gelirse `429`, karar alınamazsa `503` döner ve handler çalışmaz. `GET /v1/usage` yalnızca anahtarın sahibinin kalıcı günlük kullanımını gösterir.

Günlük kullanım **başarıyla tamamlanan iş** değil, QuotaGate'in kabul edip hak ayırdığı karardır. Handler hata verse veya istemciye yanıt ulaşmasa da ayrılan hak geri verilmez. İstemcinin örnek API isteğini yeniden göndermesi yeni bir karar ve yeni bir kullanım olabilir. Bu servis dış yan etki için exactly-once güvencesi vermez.

Servis trafik proxy'si değildir: genel routing, load balancing, TLS sertifika yönetimi, WAF, OAuth/SSO, eklenti, ödeme/faturalama, Kafka/NATS ve genel API Gateway özellikleri kapsam dışıdır. Yerel örnek API yalnızca kararın nasıl kullanıldığını gösterir. AWS, bu planın bitiş koşulu değildir.

Sürüm ve görevler: [PLAN.md](PLAN.md). Devam noktası: [STATUS.md](STATUS.md).

## QG-01 yerel çalıştırma

Gerekenler: Go 1.27+, Docker Desktop/Compose; duman testi için PowerShell 7. Mevcut kod sağlık, müşteri/politika, karar ve kullanım uçları ile örnek API'yi içerir.

```powershell
Copy-Item .env.compose.example .env.compose
# .env.compose içindeki QG_DB_PASSWORD ve QG_ADMIN_TOKEN değerlerini yalnızca bu yerel kurulum için değiştirin.
docker compose -p quotagate-qg01 --env-file .env.compose up --build -d --wait
Invoke-WebRequest http://127.0.0.1:18080/health/live
Invoke-WebRequest http://127.0.0.1:18080/health/ready
Invoke-WebRequest http://127.0.0.1:18081/health/live
docker compose -p quotagate-qg01 --env-file .env.compose run --rm migrate
docker compose -p quotagate-qg01 --env-file .env.compose down
```

`/health/live` HTTP sürecinin çalıştığını, `/health/ready` PostgreSQL'e 1 saniye içinde erişilebildiğini gösterir. Migration dosyaları `migrations/` altındadır; uygulanan dosyanın checksum'ı kaydedilir ve sonraki çalıştırmada değişmiş dosya reddedilir. `down` hacmi silmez; yerel test verisinin temizliği ayrı ve açık bir karardır. `.env.compose` Git dışında tutulur. Compose gerçek çalıştırmasının durumu [STATUS.md](STATUS.md) içinde kayıtlıdır.

## QG-02 yönetim ve müşteri uçları

Bu sürümde yönetici ve müşteri uçları `Authorization: Bearer <token>` kullanır. `ADMIN_TOKEN` yalnızca yönetim uçlarında geçerlidir. Yeni müşteri anahtarı `qg_` önekli 256 bit rastgele değerdir; yalnızca oluşturma veya yenileme yanıtında görünür, veritabanında SHA-256 hash'i bulunur. Anahtarlar ve yönetici sırrı uygulama loglarına yazılmaz. Bu yerel HTTP demosu için düz HTTP yalnızca `127.0.0.1` üzerinden yayımlanır.

| İşlem | Uç | Sonuç |
| --- | --- | --- |
| Müşteri oluştur | `POST /v1/admin/customers` `{ "name": "A" }` | `201`, müşteri kimliği ve tek seferlik anahtar |
| Politika yaz | `PUT /v1/admin/customers/{id}/policies/{operation}` `{ "daily_limit": 100, "rate_limit_per_minute": 10 }` | `200`, sürümlü politika |
| Anahtar yenile | `POST /v1/admin/customers/{id}/rotate-key` | `200`, yeni tek seferlik anahtar; eski anahtar `401` |
| Müşteriyi kapat | `POST /v1/admin/customers/{id}/disable` | `204`, mevcut anahtar `401` |
| Kendi kimliğini oku | `GET /v1/me` | `200`, anahtardan bulunan müşteri |
| Kendi politikasını oku | `GET /v1/policies/{operation}` | `200`, anahtardan bulunan müşterinin politikası |

Yönetim yolları dışındaki müşteri uçları `customer_id` almaz. Politika adı örneği `job.create` biçimindedir. QG-02 yalnızca limitleri kaydediyordu; QG-03 ile günlük karar ve kullanım eklendi.

## QG-03 günlük karar ve kullanım

Müşteri anahtarıyla `POST /v1/decisions` gövdesi `{ "decision_id": "rastgele-en-az-16-karakter", "operation": "job.create" }` gönderilir. Yanıt `200` ve `allowed`, `reason`, `utc_day`, `daily_used`, `daily_limit`, `policy_version` alanlarını taşır. `allowed=false, reason=daily_quota` karar servisi yanıtıdır; örnek API middleware'i bunu `429` olarak kullanır. Tanımsız işlem `403`, aynı karar kimliğinin farklı işlemle kullanılması `409`, geçersiz anahtar `401`, depolama kesintisi `503` verir.

`GET /v1/usage?operation=job.create`, anahtarın sahibinin **bugünkü UTC** kullanımını, limitini ve `resets_at_utc` değerini döndürür. `customer_id` parametresi kabul edilmez. Gün, PostgreSQL'in `clock_timestamp()` zamanından seçilir. Aynı müşteri ve karar kimliğiyle tekrar, ilk kaydedilen sonucu politika veya gün değişse de döndürür; yeni karar kimliği yeni bir hak talebidir.

Günlük hak, izin kararı transaction'da kaydedilince ayrılır. Sonraki handler başarısız olsa bile geri verilmez. Müşteri satırı kilidi tekrarları sıralar; koşullu PostgreSQL artışı ve karar kaydı aynı transaction'dadır. Temel `serve` modunda `/v1/decisions` günlük kota uygular; `serve-combined` modunda önce Redis hız kontrolü de uygulanır. Harici yan etkinin exactly-once çalışması vaat edilmez.

## QG-04 middleware ve örnek API

`client.New("http://127.0.0.1:18080")` ile oluşturulan Go istemcisi birden fazla müşterinin eşzamanlı isteklerinde paylaşılabilir. `decisions.Middleware("job.create", handler)` müşteri anahtarını gelen `Authorization: Bearer ...` header'ından alır; gelen gövdeyi tüketmeden QuotaGate'e tek çağrı yapar. Çağrı iki saniyeyle ve gelen isteğin iptaliyle sınırlıdır. Yönlendirmeler izlenmez; eksik, bozuk veya çelişkili karar yanıtı handler'ı çalıştırmaz. Uygulama kapanırken `decisions.Close()` boş bağlantıları kapatır.

Compose örnek API'yi `127.0.0.1:18081` üzerinde açar. `POST /jobs` izin halinde `201` ve süreç içinde numaralanmış `demo-N` iş kimliği verir. Kota reddi `429`, karar alınamaması `503`, geçersiz anahtar `401`, tanımsız işlem politikası `403` olur. Her izin handler'ı bir kez çalıştırır; diğer sonuçlarda handler çalışmaz. Gelen `Idempotency-Key` veya gövdedeki karar kimliği tekrar kullanımı sağlamaz: ayrı API isteği yeni karar ve hak talebidir.

`GET /demo/stats` tüm müşteriler için toplam `handler_calls` sayısını gösteren yerel kanıttır; örnek API yeniden başladığında sıfırlanır. Kalıcı müşteri kullanımı QuotaGate'in `GET /v1/usage?operation=job.create` ucundan alınır. Örnek iş gerçek bir işe veya harici API'ye bağlı değildir. Örnek `/health/live` yalnızca örnek HTTP sürecini gösterir; QuotaGate'in bağımlılık durumu kendi `/health/ready` ucundadır.

İki müşteri, ret, kesinti ve toparlanma demosu:

```powershell
docker compose -p quotagate-qg01 --env-file .env.compose up --build -d --wait
pwsh -File ./scripts/smoke-qg04.ps1
docker compose -p quotagate-qg01 --env-file .env.compose down
```

Betik iki geçici müşteri için günlük limit `2/3` yazar, anahtarları bellekte tutar ve aynı inbound header ile ayrı karar kimliklerini doğrular. QuotaGate `api` servisini kısa süre durdurup yeniden başlatır; ret ve kesintide handler sayacının değişmediğini kontrol eder. Sonuçları PostgreSQL sayaç/karar kayıtlarıyla uzlaştırır, uygulama loglarında sır arar ve yalnızca oluşturduğu iki müşterinin verisini temizler. Çalışan izole Compose ortamında başka trafik olmadan çalıştırın. Betik `.env.compose` içindeki portları okur; örnek API portu `QG_EXAMPLE_PORT` ile değiştirilebilir.

`go test ./... -count=1` middleware HTTP testlerini çalıştırır. Gerçek PostgreSQL testleri için ayrıca izole, migration uygulanmış veritabanını `QG_TEST_DATABASE_URL` ile verin; değişken yokken bu testler atlanır. Kabul koşularının sonuçları [STATUS.md](STATUS.md) içindedir.

## QG-05 ortak Redis ön kontrolü

`serve-rate-demo` modu, müşteri anahtarıyla `POST /demo/rate-checks` `{ "decision_id": "rastgele-en-az-16-karakter", "operation": "job.create" }` ucunu açar. Müşteri anahtardan, hız limiti PostgreSQL politikasından alınır. `200` yanıtı `allowed`, `reason=rate_allowed|rate_limited`, `rate_used`, `rate_limit`, `window_start_utc`, `resets_at_utc` alanlarını taşır. Bu bir **hız ön sonucu**dur; handler izni vermez, PostgreSQL günlük sayacını veya nihai karar tablosunu değiştirmez. Bu tarihsel demo modunda `/v1/decisions` günlük kota akışını kullanır; birleşik akış aşağıdaki `serve-combined` modundadır.

Lua script'i müşteri + işlem sayacını, Redis `TIME` değerinin düştüğü 60 saniyelik sabit pencereye göre artırır. Ret sayaç artırmaz. Sayaç anahtarının mutlak silinme zamanı pencere sonudur; müşteri + karar kimliğinin ön sonucu 24 saat tutulur. Aynı kimlik/işlem ilk izin veya ret sonucunu, eski limitini ve penceresini döndürür; tekrar saklama süresini uzatmaz. Başka işlemle aynı kimlik `409` olur. Anahtarlar müşteri/işlem/karar kimliklerinin hash'lerini kullanır; API anahtarı Redis'e verilmez. Script cache silinince `EVALSHA` → `EVAL` dönüşü vardır. [Redis Lua yürütmesi](https://redis.io/docs/latest/develop/programmability/eval-intro/), [TIME](https://redis.io/docs/latest/commands/time/) ve [PEXPIREAT](https://redis.io/docs/latest/commands/pexpireat/).

İki kopyalı yerel kabul ortamı:

```powershell
docker compose -p quotagate-qg05 --env-file .env.compose -f compose.yaml -f compose.qg05.yaml up --build -d --wait
pwsh -File ./scripts/smoke-qg05.ps1
docker compose -p quotagate-qg05 --env-file .env.compose -f compose.yaml -f compose.qg05.yaml down
```

QG-05 ayrı `quotagate-qg05_pgdata` hacmini kullanır. Varsayılan portlar birinci API `18080`, ikinci API `18082`, Redis `56379`; Redis yalnızca loopback üzerinden yayımlanır ve `QG_REDIS_PORT` ile değiştirilebilir. QG-01–04 ortamıyla aynı HTTP/DB portlarını kullandığından iki ortamı sırayla çalıştırın. Redis bu demoda parolasız, diske yazmadan ve `128mb/noeviction` sınırıyla çalışır. Redis'in durması veya veri kaybı ön sonuçları kaybettirir; kalıcı günlük kullanım PostgreSQL'de kalır. Sabit pencere sınırında iki pencerenin kapasitesi kısa bir aralıkta kullanılabilir.

Betik iki kopyaya 60 benzersiz istek gönderir ve limit 7 altında 7 ön izin/53 ret bekler; 20 eşzamanlı aynı kimlikli isteğin tek ön hak tüketmesini, müşteri/işlem ayrımını, politika değişimi sonrası ilk sonucun korunmasını ve TTL'leri doğrular. PostgreSQL günlük kullanım/karar kayıtlarının oluşmadığını ve loglarda sır bulunmadığını kontrol eder; yalnızca kendi test müşterilerini ve Redis anahtarlarını siler.

Gerçek Redis testleri için `QG_TEST_REDIS_URL=redis://127.0.0.1:56379/15` verin ve `go test ./... -count=1` çalıştırın. Testler rastgele müşteri alanlarını temizler ve script cache dönüşünü doğrulamak için **izole test Redis'inde** `SCRIPT FLUSH` kullanır. Pencere sınırı testi, üretim Lua script'inin yalnızca saat ifadesini kontrollü değerle değiştirir; ayrı test gerçek Redis `TIME` ve mutlak key expiry'yi doğrular.

## QG-06 birleşik ve kalıcı karar

`serve-combined`, aynı `POST /v1/decisions` sözleşmesinde PostgreSQL kayıtlı sonuç → Redis ön kontrol → PostgreSQL günlük kota/nihai sonuç sırasını uygular. Yanıt `200` ve `{allowed, reason, utc_day, daily_used, daily_limit, policy_version}` biçimindedir. `reason`, `allowed`, `daily_quota` ve `rate_limited` değerlerini ayırır. Redis retleri de kalıcı karar olur; günlük sayaç oluşturmaz veya artırmaz. Müşteri kimliği anahtardan alınır; yabancı `customer_id` gövdesi ve query parametreleri `400` olur.

PostgreSQL transaction'ı kayıtlı karar aranmadan önce açılır. Müşteri satırı ve güncel politika kilitleri, bağlamla sınırlı Redis çağrısı ve kalıcılaştırma boyunca tutulur. Aynı müşteri kararları sıralanır; kayıtlı sonuç bulunduğunda Redis çağrılmaz. Gün, Redis kontrolünden sonra PostgreSQL `clock_timestamp()` değerinden seçilir; günlük sayaç ve nihai kayıt birlikte commit edilir. Bu yaklaşım müşteri başına eşzamanlılığı sınırlar ve Redis gecikmesi boyunca PostgreSQL bağlantısı tutar; performans ölçümü QG-11'dedir.

| Hata veya sonuç | Redis kapasitesi | PostgreSQL günlük kullanım ve karar |
| --- | --- | --- |
| Kimlik/politika/ilk DB okuması başarısız | Redis çağrılmaz | Yeni hak veya nihai kayıt yok |
| Redis çağrısı başarısız veya yanıtı kayıp | Lua tamamlandıysa kapasite tüketilmiş olabilir | Transaction geri alınır; `503` |
| Redis hız reddi | Yeni kapasite tüketmez | Günlük artmaz; `rate_limited` kaydedilir |
| Redis izin verdi, günlük kota dolu | Tüketilen kapasite korunur | Günlük artmaz; `daily_quota` kaydedilir |
| Redis izin verdi, PostgreSQL yazımı başarısız | Tüketilen kapasite korunur | Günlük artış ve kayıt geri alınır; `503` |
| PostgreSQL commit yanıtı belirsiz veya HTTP yanıtı kayıp | Tüketilen kapasite korunur | Commit olmuş olabilir; aynı kimlikle tekrar kayıtlı sonucu bulur |
| Nihai kararın aynı müşteri/işlem/kimlikle tekrarı | Redis'e başvurulmaz | İlk gün, kullanım, limit ve sürüm döner |

İki depo arasında ortak transaction veya hız hakkı iadesi yoktur. PostgreSQL'e ulaşmamış bir ön sonucun tekrarı Redis'in 24 saatlik kaydını kullanır. Arada politika güncellendiyse bu ön sonuç eski hız limitini korur; nihai PostgreSQL kararı günlük limit/sürümü o transaction'daki güncel politikadan alır. Kalıcılaştırılmış kararlar politika değişiminden etkilenmez. Redis veri kaybında tamamlanmamış ön sonuç devamlılığı garanti edilmez.

İki kopya ve middleware için yerel kabul:

```powershell
docker compose -p quotagate-qg06 --env-file .env.compose -f compose.yaml -f compose.qg05.yaml -f compose.qg06.yaml up --build -d --wait
pwsh -NoProfile -File ./scripts/smoke-qg06.ps1
docker compose -p quotagate-qg06 --env-file .env.compose -f compose.yaml -f compose.qg05.yaml -f compose.qg06.yaml down
```

QG-06 kendi `quotagate-qg06_pgdata` hacmini kullanır. Önceki demolarla aynı portları kullanır; sırayla çalıştırın. Birleşik mod yalnızca nihai karar ucunu açar; QG-05 ön kontrol ucu bu modda yoktur. Readiness PostgreSQL ve Redis'i kontrol eder. Redis erişilemese de mevcut PostgreSQL kararının tekrarı dönebilir; yeni kararda bağımlılık hatası `503` olur.

Betik dört geçici müşteri oluşturur. İki kopyaya 60 benzersiz çağrı günlük limit `3`, hız limiti `7` ile `3 allowed / 4 daily_quota / 53 rate_limited` verir. Yirmi aynı kimlikli çağrı bir günlük/hız hakkı kullanır. Bir müşteri için geçici PostgreSQL constraint'i, Redis kontrolünden sonraki karar yazımını bozarak `503` ve korunmuş Redis kapasitesini gösterir; aynı kimlikle tekrar sonucu tamamlar. Örnek `/jobs` için hız ve günlük retleri `429` olur, handler yalnızca iki izin için çalışır. PostgreSQL kullanım/karar/izin/günlük ret/hız ret toplamı `6|67|6|6|55` ile uzlaştırılır; loglarda sır aranır. `finally` geçici constraint'i, yalnızca kendi müşterilerini ve Redis anahtarlarını temizler. Başka trafik olmayan izole ortamda çalıştırın.

Birleşik entegrasyon testleri için migration uygulanmış izole PostgreSQL'i `QG_TEST_DATABASE_URL`, test Redis'ini `QG_TEST_REDIS_URL` ile verin. Her iki değişken varsa `go test ./... -count=1 -timeout=90s`, gerçek iki bağımlılıkta yarışları, Redis yanıt kaybını, PostgreSQL INSERT hatasında günlük artışın geri alınmasını, kalıcı tekrarın Redis'i atlamasını ve HTTP yanıt kaybını doğrular. Değişkenler eksikse ilgili entegrasyon testleri atlanır. Testler yalnızca rastgele oluşturdukları müşteri verisini temizler; hata enjeksiyonu kısa süreli, müşteriye özgü bir constraint eklediğinden paylaşılan/üretim veritabanında çalıştırmayın. İstemcinin otomatik taşıma tekrarı QG-07'de eklenecek.

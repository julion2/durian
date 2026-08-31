#pragma once

#include <QObject>
#include <QNetworkAccessManager>
#include <QNetworkReply>
#include <QNetworkRequest>
#include <QJsonDocument>
#include <QJsonObject>
#include <QJsonArray>
#include <QDir>
#include <QFile>
#include <QStandardPaths>
#include <QUrl>
#include <QUrlQuery>

class NetworkClient : public QObject {
    Q_OBJECT
    Q_PROPERTY(QString baseUrl READ baseUrl WRITE setBaseUrl NOTIFY baseUrlChanged)

public:
    explicit NetworkClient(QObject *parent = nullptr)
        : QObject(parent), manager_(new QNetworkAccessManager(this)) {}

    QString baseUrl() const { return baseUrl_; }
    void setBaseUrl(const QString &url) {
        if (baseUrl_ != url) {
            baseUrl_ = url;
            emit baseUrlChanged();
        }
    }

    Q_INVOKABLE void search(const QString &query, int limit = 50) {
        QUrl url(baseUrl_ + "/api/v1/search");
        QUrlQuery params;
        params.addQueryItem("query", query);
        params.addQueryItem("limit", QString::number(limit));
        params.addQueryItem("enrich", QString::number(limit));
        url.setQuery(params);

        auto *reply = manager_->get(QNetworkRequest(url));
        connect(reply, &QNetworkReply::finished, this, [this, reply]() {
            reply->deleteLater();
            if (reply->error() != QNetworkReply::NoError) {
                emit searchError(reply->errorString());
                return;
            }
            auto doc = QJsonDocument::fromJson(reply->readAll());
            auto obj = doc.object();
            if (!obj.value("ok").toBool()) {
                emit searchError("API returned error");
                return;
            }
            // Merge preview text from enriched threads into results
            auto results = obj.value("results").toArray();
            auto threads = obj.value("threads").toObject();
            QJsonArray enriched;
            for (const auto &val : results) {
                auto r = val.toObject();
                auto tid = r.value("thread_id").toString();
                if (threads.contains(tid)) {
                    auto msgs = threads.value(tid).toObject()
                                    .value("messages").toArray();
                    if (!msgs.isEmpty()) {
                        auto body = msgs.first().toObject().value("body").toString();
                        r.insert("preview", body.left(200).simplified());
                    }
                    // Check for real attachments across all messages
                    bool hasAttachment = false;
                    for (const auto &m : msgs) {
                        auto atts = m.toObject().value("attachments").toArray();
                        for (const auto &a : atts) {
                            if (a.toObject().value("disposition").toString() != "inline") {
                                hasAttachment = true;
                                break;
                            }
                        }
                        if (hasAttachment) break;
                    }
                    if (hasAttachment) r.insert("hasAttachment", true);
                }
                enriched.append(r);
            }
            emit searchResults(enriched);
        });
    }

    Q_INVOKABLE void quickSearch(const QString &query, int limit = 25) {
        QUrl url(baseUrl_ + "/api/v1/search");
        QUrlQuery params;
        params.addQueryItem("query", query);
        params.addQueryItem("limit", QString::number(limit));
        params.addQueryItem("enrich", QString::number(limit));
        url.setQuery(params);

        auto *reply = manager_->get(QNetworkRequest(url));
        connect(reply, &QNetworkReply::finished, this, [this, reply]() {
            reply->deleteLater();
            if (reply->error() != QNetworkReply::NoError) return;
            auto doc = QJsonDocument::fromJson(reply->readAll());
            auto obj = doc.object();
            if (!obj.value("ok").toBool()) return;
            // Merge preview from enriched threads
            auto results = obj.value("results").toArray();
            auto threads = obj.value("threads").toObject();
            QJsonArray enriched;
            for (const auto &val : results) {
                auto r = val.toObject();
                auto tid = r.value("thread_id").toString();
                if (threads.contains(tid)) {
                    auto msgs = threads.value(tid).toObject()
                                    .value("messages").toArray();
                    if (!msgs.isEmpty()) {
                        auto body = msgs.first().toObject().value("body").toString();
                        r.insert("preview", body.left(200).simplified());
                    }
                }
                enriched.append(r);
            }
            emit quickSearchResults(enriched);
        });
    }

    Q_INVOKABLE void downloadAttachment(const QString &messageIdentifier, int partId,
                                           const QString &filename, const QString &savePath) {
        const auto encodedIdentifier = QString::fromLatin1(QUrl::toPercentEncoding(messageIdentifier));
        QUrl url(baseUrl_ + "/api/v1/messages/" + encodedIdentifier + "/attachments/" + QString::number(partId));
        auto *reply = manager_->get(QNetworkRequest(url));
        connect(reply, &QNetworkReply::finished, this, [this, reply, savePath, filename]() {
            reply->deleteLater();
            if (reply->error() != QNetworkReply::NoError) {
                emit downloadError(filename, reply->errorString());
                return;
            }
            QByteArray data = reply->readAll();
            QString path = savePath;
            if (QFileInfo(path).isDir())
                path = path + "/" + filename;
            QFile file(path);
            if (file.open(QIODevice::WriteOnly)) {
                file.write(data);
                file.close();
                emit downloadComplete(filename, path);
            } else {
                emit downloadError(filename, "Could not write to " + path);
            }
        });
    }

    Q_INVOKABLE QString downloadsPath() const {
        return QStandardPaths::writableLocation(QStandardPaths::DownloadLocation);
    }

    Q_INVOKABLE void fetchThread(const QString &threadId) {
        QUrl url(baseUrl_ + "/api/v1/threads/" + threadId);
        auto *reply = manager_->get(QNetworkRequest(url));
        connect(reply, &QNetworkReply::finished, this, [this, reply]() {
            reply->deleteLater();
            if (reply->error() != QNetworkReply::NoError) {
                emit searchError(reply->errorString());
                return;
            }
            auto doc = QJsonDocument::fromJson(reply->readAll());
            auto obj = doc.object();
            if (!obj.value("ok").toBool()) {
                emit searchError("API returned error");
                return;
            }
            auto thread = obj.value("thread").toObject();
            emit threadLoaded(thread);
        });
    }

signals:
    void baseUrlChanged();
    void searchResults(const QJsonArray &results);
    void quickSearchResults(const QJsonArray &results);
    void threadLoaded(const QJsonObject &thread);
    void searchError(const QString &error);
    void downloadComplete(const QString &filename, const QString &path);
    void downloadError(const QString &filename, const QString &error);

private:
    QNetworkAccessManager *manager_;
    QString baseUrl_ = "http://localhost:9723";
};
